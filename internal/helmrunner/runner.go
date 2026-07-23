// Package helmrunner wraps the Helm v4 Go SDK (helm.sh/helm/v4/pkg/action)
// directly, in-process. It never shells out to a helm binary, so there is no
// command line to construct and no injection surface from caller-supplied
// strings.
package helmrunner

import (
	"context"
	"fmt"
	"time"

	"k8s.io/cli-runtime/pkg/genericclioptions"

	"helm.sh/helm/v4/pkg/action"
	helmchart "helm.sh/helm/v4/pkg/chart"
	"helm.sh/helm/v4/pkg/chart/loader"
	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/kube"
	"helm.sh/helm/v4/pkg/registry"
	release "helm.sh/helm/v4/pkg/release/v1"
)

// Runner executes Helm operations against the in-cluster Kubernetes API.
//
// settings is created once and is only ever read from after New() returns:
// callers must not mutate it. Per-request Kubernetes client configuration
// (namespace in particular) is built fresh for every call so that concurrent
// requests targeting different namespaces cannot race on shared state.
type Runner struct {
	settings *cli.EnvSettings
}

// New builds a Runner. It relies on in-cluster service account credentials
// (the standard client-go fallback) unless KUBECONFIG is set.
func New() *Runner {
	return &Runner{settings: cli.New()}
}

// ChartRef identifies a chart the same way `helm install` does: a local
// path, an http(s) URL, a repo-relative name with RepoURL, or an oci:// ref.
type ChartRef struct {
	Chart   string `json:"chart"`
	Version string `json:"version,omitempty"`
	RepoURL string `json:"repoUrl,omitempty"`
	Devel   bool   `json:"devel,omitempty"`
}

func (r *Runner) restClientGetter(namespace string) genericclioptions.RESTClientGetter {
	cf := genericclioptions.NewConfigFlags(true)
	cf.Namespace = &namespace
	return cf
}

func (r *Runner) registryClient() (*registry.Client, error) {
	return registry.NewClient(
		registry.ClientOptEnableCache(true),
		registry.ClientOptCredentialsFile(r.settings.RegistryConfig),
	)
}

func (r *Runner) newConfiguration(namespace string) (*action.Configuration, error) {
	cfg := action.NewConfiguration()

	rc, err := r.registryClient()
	if err != nil {
		return nil, fmt.Errorf("build registry client: %w", err)
	}
	cfg.RegistryClient = rc

	if err := cfg.Init(r.restClientGetter(namespace), namespace, "secret"); err != nil {
		return nil, fmt.Errorf("init helm configuration: %w", err)
	}
	return cfg, nil
}

func waitStrategy(wait bool) kube.WaitStrategy {
	if wait {
		return kube.StatusWatcherStrategy
	}
	return kube.HookOnlyStrategy
}

func timeoutOrDefault(d time.Duration) time.Duration {
	if d <= 0 {
		return 5 * time.Minute
	}
	return d
}

func asRelease(v any) (*release.Release, error) {
	rel, ok := v.(*release.Release)
	if !ok {
		return nil, fmt.Errorf("unexpected release type %T from helm SDK", v)
	}
	return rel, nil
}

// InstallRequest describes a `helm install` invocation.
type InstallRequest struct {
	ChartRef
	ReleaseName     string         `json:"releaseName"`
	Namespace       string         `json:"namespace"`
	CreateNamespace bool           `json:"createNamespace,omitempty"`
	Wait            bool           `json:"wait,omitempty"`
	Timeout         time.Duration  `json:"timeout,omitempty"`
	Values          map[string]any `json:"values,omitempty"`
}

// Install runs `helm install` via the SDK.
func (r *Runner) Install(ctx context.Context, req InstallRequest) (*release.Release, error) {
	cfg, err := r.newConfiguration(req.Namespace)
	if err != nil {
		return nil, err
	}

	client := action.NewInstall(cfg)
	client.ReleaseName = req.ReleaseName
	client.Namespace = req.Namespace
	client.CreateNamespace = req.CreateNamespace
	client.Version = req.Version
	client.RepoURL = req.RepoURL
	client.Devel = req.Devel
	client.Timeout = timeoutOrDefault(req.Timeout)
	client.WaitStrategy = waitStrategy(req.Wait)

	chrt, err := r.loadChart(client.ChartPathOptions, req.Chart)
	if err != nil {
		return nil, err
	}

	rel, err := client.RunWithContext(ctx, chrt, req.Values)
	if err != nil {
		return nil, err
	}
	return asRelease(rel)
}

// UpgradeRequest describes a `helm upgrade` invocation.
type UpgradeRequest struct {
	ChartRef
	ReleaseName string         `json:"releaseName"`
	Namespace   string         `json:"namespace"`
	Install     bool           `json:"install,omitempty"` // mirrors `helm upgrade --install`
	Wait        bool           `json:"wait,omitempty"`
	Timeout     time.Duration  `json:"timeout,omitempty"`
	Values      map[string]any `json:"values,omitempty"`
	ResetValues bool           `json:"resetValues,omitempty"`
	ReuseValues bool           `json:"reuseValues,omitempty"`
	MaxHistory  int            `json:"maxHistory,omitempty"`
}

// Upgrade runs `helm upgrade` via the SDK. If Install is true and the
// release does not exist yet, it falls back to an install, matching
// `helm upgrade --install`.
func (r *Runner) Upgrade(ctx context.Context, req UpgradeRequest) (*release.Release, error) {
	cfg, err := r.newConfiguration(req.Namespace)
	if err != nil {
		return nil, err
	}

	if req.Install {
		histClient := action.NewHistory(cfg)
		histClient.Max = 1
		if _, histErr := histClient.Run(req.ReleaseName); histErr != nil {
			installReq := InstallRequest{
				ChartRef:        req.ChartRef,
				ReleaseName:     req.ReleaseName,
				Namespace:       req.Namespace,
				CreateNamespace: true,
				Wait:            req.Wait,
				Timeout:         req.Timeout,
				Values:          req.Values,
			}
			return r.Install(ctx, installReq)
		}
	}

	client := action.NewUpgrade(cfg)
	client.Install = req.Install
	client.Namespace = req.Namespace
	client.Version = req.Version
	client.RepoURL = req.RepoURL
	client.Devel = req.Devel
	client.Timeout = timeoutOrDefault(req.Timeout)
	client.WaitStrategy = waitStrategy(req.Wait)
	client.ResetValues = req.ResetValues
	client.ReuseValues = req.ReuseValues
	if req.MaxHistory > 0 {
		client.MaxHistory = req.MaxHistory
	}

	chrt, err := r.loadChart(client.ChartPathOptions, req.Chart)
	if err != nil {
		return nil, err
	}

	rel, err := client.RunWithContext(ctx, req.ReleaseName, chrt, req.Values)
	if err != nil {
		return nil, err
	}
	return asRelease(rel)
}

// UninstallRequest describes a `helm uninstall` invocation.
type UninstallRequest struct {
	ReleaseName string        `json:"releaseName"`
	Namespace   string        `json:"namespace"`
	KeepHistory bool          `json:"keepHistory,omitempty"`
	Wait        bool          `json:"wait,omitempty"`
	Timeout     time.Duration `json:"timeout,omitempty"`
}

// UninstallResult reports what happened to the release.
type UninstallResult struct {
	ReleaseName string `json:"releaseName"`
	Namespace   string `json:"namespace"`
	Info        string `json:"info,omitempty"`
}

// Uninstall runs `helm uninstall` via the SDK.
func (r *Runner) Uninstall(ctx context.Context, req UninstallRequest) (*UninstallResult, error) {
	cfg, err := r.newConfiguration(req.Namespace)
	if err != nil {
		return nil, err
	}

	client := action.NewUninstall(cfg)
	client.KeepHistory = req.KeepHistory
	client.Timeout = timeoutOrDefault(req.Timeout)
	client.WaitStrategy = waitStrategy(req.Wait)

	resp, err := client.Run(req.ReleaseName)
	if err != nil {
		return nil, err
	}

	result := &UninstallResult{ReleaseName: req.ReleaseName, Namespace: req.Namespace}
	if resp != nil {
		result.Info = resp.Info
	}
	return result, nil
}

// StatusRequest describes a `helm status` invocation.
type StatusRequest struct {
	ReleaseName string `json:"releaseName"`
	Namespace   string `json:"namespace"`
	Revision    int    `json:"revision,omitempty"`
}

// Status runs `helm status` via the SDK.
func (r *Runner) Status(_ context.Context, req StatusRequest) (*release.Release, error) {
	cfg, err := r.newConfiguration(req.Namespace)
	if err != nil {
		return nil, err
	}

	client := action.NewStatus(cfg)
	if req.Revision > 0 {
		client.Version = req.Revision
	}

	rel, err := client.Run(req.ReleaseName)
	if err != nil {
		return nil, err
	}
	return asRelease(rel)
}

// TemplateRequest describes a `helm template` invocation. It never contacts
// the cluster to mutate anything (client-side dry run only).
type TemplateRequest struct {
	ChartRef
	ReleaseName string         `json:"releaseName,omitempty"`
	Namespace   string         `json:"namespace,omitempty"`
	IncludeCRDs bool           `json:"includeCrds,omitempty"`
	Values      map[string]any `json:"values,omitempty"`
}

// Template renders a chart locally, equivalent to `helm template`.
func (r *Runner) Template(ctx context.Context, req TemplateRequest) (*release.Release, error) {
	namespace := req.Namespace
	if namespace == "" {
		namespace = "default"
	}

	cfg, err := r.newConfiguration(namespace)
	if err != nil {
		return nil, err
	}

	client := action.NewInstall(cfg)
	client.DryRunStrategy = action.DryRunClient
	client.ReleaseName = req.ReleaseName
	if client.ReleaseName == "" {
		client.ReleaseName = "release-name"
	}
	client.Replace = true
	client.Namespace = namespace
	client.Version = req.Version
	client.RepoURL = req.RepoURL
	client.Devel = req.Devel
	client.IncludeCRDs = req.IncludeCRDs
	client.WaitStrategy = kube.HookOnlyStrategy

	chrt, err := r.loadChart(client.ChartPathOptions, req.Chart)
	if err != nil {
		return nil, err
	}

	rel, err := client.RunWithContext(ctx, chrt, req.Values)
	if err != nil {
		return nil, err
	}
	return asRelease(rel)
}

// LintRequest describes a `helm lint` invocation.
type LintRequest struct {
	ChartRef
	Namespace string         `json:"namespace,omitempty"`
	Strict    bool           `json:"strict,omitempty"`
	Values    map[string]any `json:"values,omitempty"`
}

// LintResult reports lint findings.
type LintResult struct {
	Passed   bool     `json:"passed"`
	Messages []string `json:"messages,omitempty"`
	Errors   []string `json:"errors,omitempty"`
}

// Lint runs `helm lint` via the SDK. It never contacts the cluster.
func (r *Runner) Lint(req LintRequest) (*LintResult, error) {
	cpo := action.ChartPathOptions{Version: req.Version, RepoURL: req.RepoURL}
	cp, err := cpo.LocateChart(req.Chart, r.settings)
	if err != nil {
		return nil, fmt.Errorf("locate chart: %w", err)
	}

	client := action.NewLint()
	client.Strict = req.Strict
	client.Namespace = req.Namespace

	res := client.Run([]string{cp}, req.Values)

	out := &LintResult{Passed: len(res.Errors) == 0}
	for _, m := range res.Messages {
		out.Messages = append(out.Messages, m.Error())
	}
	for _, e := range res.Errors {
		out.Errors = append(out.Errors, e.Error())
	}
	return out, nil
}

func (r *Runner) loadChart(cpo action.ChartPathOptions, chartRef string) (helmchart.Charter, error) {
	cp, err := cpo.LocateChart(chartRef, r.settings)
	if err != nil {
		return nil, fmt.Errorf("locate chart %q: %w", chartRef, err)
	}
	chrt, err := loader.Load(cp)
	if err != nil {
		return nil, fmt.Errorf("load chart %q: %w", chartRef, err)
	}
	return chrt, nil
}
