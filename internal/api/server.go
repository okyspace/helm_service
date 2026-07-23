// Package api exposes the Helm sidecar's operations as HTTP/JSON over
// TCP, bound to 127.0.0.1 only. There is no shell involved anywhere in
// this path: requests are decoded straight into typed Go structs and
// passed to the helmrunner package, which calls the Helm SDK in-process.
//
// Binding to loopback relies on the Kubernetes property that all
// containers in a pod share one network namespace: 127.0.0.1 inside this
// container is reachable by every other container in the same pod, and by
// nothing outside it, without any Kubernetes Service/NetworkPolicy needed.
// That also means the port is ambiently reachable by *any* container in
// the pod (unlike a Unix socket scoped to a specific volume mount) -- see
// the README's Security section for that tradeoff.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"time"

	"ainexushub.local/helm-sidecar/internal/helmrunner"
)

const (
	maxRequestBody = 1 << 20 // 1 MiB: chart refs + values, not chart content itself
	requestHardCap = 15 * time.Minute
	shutdownGrace  = 10 * time.Second
	readHeaderCap  = 5 * time.Second
)

// dns1123 matches Kubernetes object-name-like tokens (release names,
// namespaces): lowercase alphanumeric and '-', bounded length. This is
// input-shape validation, not an injection defense -- nothing here reaches
// a shell -- but it fails fast on malformed requests with a clear 400
// instead of a confusing error surfaced from deep inside the SDK.
var dns1123 = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,251}[a-z0-9])?$`)

// Server serves the sidecar's HTTP/JSON API over TCP on loopback.
type Server struct {
	runner *helmrunner.Runner
	log    *slog.Logger
	http   *http.Server
}

// New builds a Server. log may be nil, in which case slog.Default() is used.
func New(runner *helmrunner.Runner, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{runner: runner, log: log}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("POST /v1/install", s.handleInstall)
	mux.HandleFunc("POST /v1/upgrade", s.handleUpgrade)
	mux.HandleFunc("POST /v1/uninstall", s.handleUninstall)
	mux.HandleFunc("POST /v1/status", s.handleStatus)
	mux.HandleFunc("POST /v1/template", s.handleTemplate)
	mux.HandleFunc("POST /v1/lint", s.handleLint)

	s.http = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: readHeaderCap,
	}
	return s
}

// ListenAndServe binds TCP at addr (expected to be a loopback address,
// e.g. "127.0.0.1:8080") and serves until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	lc := net.ListenConfig{}
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- s.http.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		_ = s.http.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func decode[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var v T
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode request: %w", err))
		var zero T
		return zero, false
	}
	return v, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, errorResponse{Error: err.Error()})
}

func requestContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), requestHardCap)
}

func validName(field, value string) error {
	if !dns1123.MatchString(value) {
		return fmt.Errorf("%s %q is not a valid Kubernetes name", field, value)
	}
	return nil
}

func nonEmpty(field, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", field)
	}
	return nil
}

func (s *Server) handleInstall(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[helmrunner.InstallRequest](w, r)
	if !ok {
		return
	}
	if err := errors.Join(
		nonEmpty("chart", req.Chart),
		nonEmpty("releaseName", req.ReleaseName),
		nonEmpty("namespace", req.Namespace),
		validName("releaseName", req.ReleaseName),
		validName("namespace", req.Namespace),
	); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	ctx, cancel := requestContext(r)
	defer cancel()

	rel, err := s.runner.Install(ctx, req)
	if err != nil {
		s.log.Error("install failed", "release", req.ReleaseName, "namespace", req.Namespace, "error", err)
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusOK, toReleaseView(rel))
}

func (s *Server) handleUpgrade(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[helmrunner.UpgradeRequest](w, r)
	if !ok {
		return
	}
	if err := errors.Join(
		nonEmpty("chart", req.Chart),
		nonEmpty("releaseName", req.ReleaseName),
		nonEmpty("namespace", req.Namespace),
		validName("releaseName", req.ReleaseName),
		validName("namespace", req.Namespace),
	); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	ctx, cancel := requestContext(r)
	defer cancel()

	rel, err := s.runner.Upgrade(ctx, req)
	if err != nil {
		s.log.Error("upgrade failed", "release", req.ReleaseName, "namespace", req.Namespace, "error", err)
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusOK, toReleaseView(rel))
}

func (s *Server) handleUninstall(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[helmrunner.UninstallRequest](w, r)
	if !ok {
		return
	}
	if err := errors.Join(
		nonEmpty("releaseName", req.ReleaseName),
		nonEmpty("namespace", req.Namespace),
		validName("releaseName", req.ReleaseName),
		validName("namespace", req.Namespace),
	); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	ctx, cancel := requestContext(r)
	defer cancel()

	res, err := s.runner.Uninstall(ctx, req)
	if err != nil {
		s.log.Error("uninstall failed", "release", req.ReleaseName, "namespace", req.Namespace, "error", err)
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[helmrunner.StatusRequest](w, r)
	if !ok {
		return
	}
	if err := errors.Join(
		nonEmpty("releaseName", req.ReleaseName),
		nonEmpty("namespace", req.Namespace),
		validName("releaseName", req.ReleaseName),
		validName("namespace", req.Namespace),
	); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	ctx, cancel := requestContext(r)
	defer cancel()

	rel, err := s.runner.Status(ctx, req)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, toReleaseView(rel))
}

func (s *Server) handleTemplate(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[helmrunner.TemplateRequest](w, r)
	if !ok {
		return
	}
	if err := nonEmpty("chart", req.Chart); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	ctx, cancel := requestContext(r)
	defer cancel()

	rel, err := s.runner.Template(ctx, req)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusOK, toReleaseView(rel))
}

func (s *Server) handleLint(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[helmrunner.LintRequest](w, r)
	if !ok {
		return
	}
	if err := nonEmpty("chart", req.Chart); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	res, err := s.runner.Lint(req)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
