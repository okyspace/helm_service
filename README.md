# Helm Service

A Kubernetes service that gives another workload the ability to run Helm
install/upgrade/uninstall/status/template/lint — **without a shell, a
`helm` binary, or a subprocess anywhere in the path**.

It links the [Helm v4 Go SDK](https://pkg.go.dev/helm.sh/helm/v4/pkg/action)
directly and exposes it as an HTTP/JSON API over TCP. Nothing on either
side ever builds a command line or execs a process, so there is no
command-injection surface between caller and callee.

The same image runs in two modes, chosen by `HELM_SIDECAR_MODE`:

| Mode | `HELM_SIDECAR_MODE` | Binds | Runs as |
|---|---|---|---|
| **sidecar** (default) | `sidecar` | `127.0.0.1:8080` | a second container in *your* pod |
| **deployment** | `deployment` | `0.0.0.0:8080` | its own standalone Deployment, fronted by a Service |

See [`deploy/`](deploy/) for mode-specific docs, examples, and — for
deployment mode — a Helm chart.

## Versions

| Component | Version | Where it's pinned |
|---|---|---|
| Go toolchain | 1.26 | [`go.mod`](go.mod) (`go 1.26.0`), builder image tag in [`Containerfile`](Containerfile) |
| Helm SDK | v4.2.3 (`helm.sh/helm/v4`) | [`go.mod`](go.mod) |
| `k8s.io/cli-runtime` | v0.36.1 | [`go.mod`](go.mod) (transitively pins the rest of `k8s.io/*` to `v0.36.1`) |
| Base image | `gcr.io/distroless/static-debian12:nonroot` | [`Containerfile`](Containerfile) |

Current built image: **~63 MB**, no shell, no package manager, runs as
`nonroot:nonroot` (uid/gid 65532).

This project's own release version (distinct from the dependency versions
above) lives in [`VERSION`](VERSION) - a single semver line, currently
`0.1.0`. `build.sh` reads it for two things: the image's default tag (still
overridable via `TAG=...`), and the `VERSION` build-arg baked into the
binary via `-ldflags -X main.version=...` (see [`Containerfile`](Containerfile)),
which the running binary reports at startup (in its JSON log line) and via
`GET /version` (see [Using the API](#using-the-api)). A plain `go build`
outside that pipeline leaves it at the fallback `"dev"`. Bump `VERSION`
together with [`deploy/deployment/Chart.yaml`](deploy/deployment/Chart.yaml)'s
`version`/`appVersion` and [`values.yaml`](deploy/deployment/values.yaml)'s
`image.tag`, which default to the same value.

## Why Helm v4, and why distroless/static

- Helm's Go SDK compiles to a fully static binary (`CGO_ENABLED=0`), so it
  has no libc dependency — `distroless/static` (not the larger
  `distroless/base`) is sufficient. That's most of the CVE-surface argument
  for distroless: there's no OS package tree to patch, only the Go module
  graph.
- As of v4, Helm's own `go.mod` no longer needs `replace` directives copied
  into consumers — the classic pain point of embedding Helm as a library.
  `go mod tidy` resolves cleanly against the versions above.
- `distroless/static-debian12` ships CA certificates and a `nonroot` user
  out of the box, which is exactly what's needed to talk TLS to the
  Kubernetes API and OCI registries as a non-root process.

## How it works

Sidecar mode (default):

```
┌─────────────────────── pod (one network namespace) ───────────────┐
│  ┌─────────────────┐                    ┌────────────────────┐    │
│  │  your container  │  HTTP/JSON         │   helm-sidecar      │    │
│  │  (any language)  │──▶ 127.0.0.1:8080 ▶│   (this image)      │    │
│  └─────────────────┘                    └────────────────────┘    │
└─────────────────────────────────────────────────────────────────┘
```

Deployment mode (`HELM_SIDECAR_MODE=deployment`):

```
caller pod(s), any namespace
        │  HTTP/JSON
        ▼
helm-sidecar Service (ClusterIP:8080)
        │
        ▼
helm-sidecar Deployment pod, listening 0.0.0.0:8080
```

- `internal/helmrunner` wraps `action.Install` / `action.Upgrade` /
  `action.Uninstall` / `action.Status` / `action.NewInstall` (dry-run, for
  `template`) / `action.NewLint`. Every request builds its own
  `action.Configuration` and `genericclioptions.RESTClientGetter`, so
  concurrent requests targeting different namespaces can't race on shared
  state. This layer is identical in both modes.
- `internal/api` is a plain `net/http` server (stdlib only — no gRPC, no
  third-party HTTP framework). Requests are strict JSON
  (`DisallowUnknownFields`), capped at 1 MiB, release/namespace names are
  validated against the Kubernetes DNS-1123 pattern before touching the
  SDK. This layer is also identical in both modes — only the bind address
  it's handed differs, see [Security: modes and bind address](#security-modes-and-bind-address)
  below.
- Kubernetes auth is whatever the pod's ServiceAccount provides (in-cluster
  config, the standard client-go fallback) unless `KUBECONFIG` is set.

## Folder structure

```
cmd/, internal/, Containerfile, build.sh, go.mod   common to both modes:
                                                    one Go binary, one image
deploy/
  sidecar/       sidecar-mode docs + example pod fragment (no chart: the
                 container is added directly to your own pod spec)
  deployment/    deployment-mode Helm chart (Chart.yaml, values.yaml,
                 templates/) that installs helm-sidecar as its own
                 Deployment + Service
test/            end-to-end harness exercising the API (sidecar mode)
                 against test/hello-world, a throwaway chart used only as
                 test fixture data — unrelated to deploy/deployment's chart
```

## Security: modes and bind address

This started as a Unix domain socket on a shared `emptyDir`, then was
switched to loopback TCP, and now optionally to a pod-network bind for
deployment mode. The bind address is never taken from an arbitrary env
var — `HELM_SIDECAR_MODE` selects between exactly two hardcoded addresses
in [`cmd/helmsidecar/main.go`](cmd/helmsidecar/main.go), so a stray env var
can't widen it to something unintended.

- **Unix socket** (an earlier design, no longer used): reachable only by
  containers that explicitly mount the volume holding the socket file, and
  further gated by the socket's file permission bits (`0660` + matching
  `runAsGroup`/`fsGroup`). A container with no reason to talk to Helm, and
  no volume mount for it, structurally cannot reach it.
- **Sidecar mode** (`127.0.0.1`, default): every container in the pod
  shares one network namespace, so the port is reachable by *any* container
  in the pod — including anything added later (a service-mesh sidecar, a
  `kubectl debug` ephemeral container) — with no extra configuration, but
  nothing *outside* the pod can reach it. In exchange, there's no
  shared-volume/permission-bit plumbing to get wrong, which was the main
  friction of the socket approach outside Kubernetes (plain `docker run`
  has no `fsGroup` equivalent).
- **Deployment mode** (`0.0.0.0`): gives up the pod-local guarantee
  entirely — the port is reachable by anything in the cluster that can
  route to the fronting Service, and the HTTP API itself does not
  authenticate callers. RBAC on the ServiceAccount and, ideally, a
  `NetworkPolicy` restricting who may reach the Service are what bound the
  blast radius here — see [`deploy/deployment/README.md#security`](deploy/deployment/README.md#security).

## Building

```bash
cd installers/helm
./build.sh                       # -> helm-sidecar:<contents of VERSION>
REGISTRY=harbor.local/ainexushub TAG=v1.0.0 PUSH=true ./build.sh
SCAN=true ./build.sh             # also runs `trivy image` if installed
BUILDER=docker ./build.sh        # force docker over podman if both are present
```

Env vars `build.sh` reads: `REGISTRY`, `IMAGE_NAME` (default
`helm-sidecar`), `TAG` (default: contents of [`VERSION`](VERSION)),
`GO_VERSION` (default `1.26`), `BUILDER` (default: `podman` if installed,
else `docker`), `PUSH`, `SCAN`. Regardless of `BUILDER`/`TAG`, the binary
itself always gets built with the real `VERSION` file contents baked in
via `-ldflags -X main.version=...` (see [Versions](#versions) above) - the
two are independent so a CI run tagging the image `:ci` still reports its
real semver at `/version`.

## Deploying

Both modes need a ServiceAccount with RBAC in whatever namespaces it will
manage — at minimum, read/write on `secrets` (Helm's default release
storage driver) plus whatever permissions the charts it installs actually
need. Beyond that, the two modes differ:

- **Sidecar** — add the container directly to your existing pod spec; see
  [`deploy/sidecar/README.md`](deploy/sidecar/README.md) and
  [`deploy/sidecar/examples/pod.yaml`](deploy/sidecar/examples/pod.yaml).
- **Deployment** — install the chart at
  [`deploy/deployment/`](deploy/deployment/README.md), e.g.
  `helm install helm-sidecar ./deploy/deployment`.

## Using the API

All endpoints are POST with a JSON body, except `GET /healthz` and
`GET /version`. The API itself is identical in both modes; only the
address differs. Examples below assume sidecar mode, i.e. the calling
container reaches helm-sidecar at `http://127.0.0.1:8080` (the pod-shared
loopback address). In deployment mode, replace that with the Service's
cluster-DNS name, e.g. `http://helm-sidecar.helm-sidecar.svc.cluster.local:8080`.

```bash
# health check
curl http://127.0.0.1:8080/healthz

# what's actually running (see VERSION and Versions above)
curl http://127.0.0.1:8080/version   # {"version":"0.1.0"}

# install
curl -X POST -H 'Content-Type: application/json' \
  -d '{
        "chart": "oci://harbor.local/charts/myapp",
        "version": "1.2.3",
        "releaseName": "myapp",
        "namespace": "myapp-ns",
        "createNamespace": true,
        "wait": true,
        "values": {"replicaCount": 2}
      }' \
  http://127.0.0.1:8080/v1/install

# upgrade (with --install semantics)
curl -X POST -H 'Content-Type: application/json' \
  -d '{"chart":"oci://harbor.local/charts/myapp","version":"1.3.0",
       "releaseName":"myapp","namespace":"myapp-ns","install":true}' \
  http://127.0.0.1:8080/v1/upgrade

# status
curl -X POST -H 'Content-Type: application/json' \
  -d '{"releaseName":"myapp","namespace":"myapp-ns"}' \
  http://127.0.0.1:8080/v1/status

# uninstall
curl -X POST -H 'Content-Type: application/json' \
  -d '{"releaseName":"myapp","namespace":"myapp-ns"}' \
  http://127.0.0.1:8080/v1/uninstall

# template (client-side render, no cluster mutation)
curl -X POST -H 'Content-Type: application/json' \
  -d '{"chart":"oci://harbor.local/charts/myapp","version":"1.2.3"}' \
  http://127.0.0.1:8080/v1/template

# lint (no cluster contact at all)
curl -X POST -H 'Content-Type: application/json' \
  -d '{"chart":"/charts/myapp"}' \
  http://127.0.0.1:8080/v1/lint
```

Install/upgrade/status responses are a trimmed `ReleaseView` (name,
namespace, revision, status, notes, manifest, chart name/version) — see
[`internal/api/types.go`](internal/api/types.go). Full request field
reference is the `*Request` structs in
[`internal/helmrunner/runner.go`](internal/helmrunner/runner.go).

## Testing

[`test/hello-world`](test/hello-world) is a minimal chart (Deployment,
Secret, ConfigMap, PVC, ServiceAccount, Service, Ingress) for exercising
the API end to end without needing a real workload. It has no cluster
dependencies beyond a default StorageClass and an ingress controller, so
`lint` and `template` work anywhere, and `install` works on any cluster.

Plain `docker run` gives each container its own network namespace by
default, so to actually mirror pod networking (both containers sharing one
loopback), start the sidecar first and run the caller with
`--network container:<sidecar-name>`:

```bash
docker run -d --name helm-sidecar-dev \
  -v "$(pwd)/test/hello-world:/charts/hello-world:ro" \
  helm-sidecar:latest

docker run --rm --network container:helm-sidecar-dev curlimages/curl:latest \
  -s -X POST -H 'Content-Type: application/json' \
  -d '{"chart":"/charts/hello-world"}' \
  http://127.0.0.1:8080/v1/lint

docker run --rm --network container:helm-sidecar-dev curlimages/curl:latest \
  -s -X POST -H 'Content-Type: application/json' \
  -d '{"chart":"/charts/hello-world","releaseName":"hello","namespace":"default"}' \
  http://127.0.0.1:8080/v1/template
```

A plain `docker run -p 8080:8080` published port will **not** reach it —
that's expected, and confirms the loopback bind is doing its job: Docker's
port publishing NATs to the container's external interface, not into its
loopback namespace, the same way a Kubernetes `Service` can't accidentally
expose this port outside the pod either.

## Code quality

[`golangci-lint`](https://golangci-lint.run) (config: [`.golangci.yml`](.golangci.yml))
is the linter for this repo, run as `golangci-lint run ./...`. It requires a
build built with Go ≥ the version in [`go.mod`](go.mod) — `go install
github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest` picks up
whatever toolchain is on `PATH` (or set `GOTOOLCHAIN` explicitly) if your
installed binary predates the module's Go version.

Rather than the `all` preset (deliberately not meant for regular use —
several of its linters actively fight idiomatic Go, e.g. banning the
`if err := f(); err != nil` form used throughout this codebase, or
demanding static sentinel errors for one-off validation messages returned
verbatim to HTTP callers), the enabled set is deliberately small and
targeted at what actually matters for this codebase:

- `errcheck`, `govet`, `ineffassign`, `staticcheck`, `unused` — the
  default set: unchecked errors, suspicious constructs, dead
  assignments/code.
- `gosec` — security-focused static analysis, given the project's whole
  premise is a privileged, cluster-credentialed process; it currently
  reports zero findings.
- `revive` — general correctness/style (e.g. caught an unused `context.Context`
  parameter inconsistent with the file's own convention elsewhere).
- `gocritic` — caught a real bug: `os.Exit` after a `defer` in `main` meant
  the deferred `signal.NotifyContext` cleanup would never run on either
  exit path (fixed by extracting `run() int` so `os.Exit` only happens once,
  after all defers have executed).
- `modernize` — Go-version-aware suggestions; caught `omitempty` on a
  `time.Time` field having no effect (structs are never "empty" to
  `encoding/json`'s classic `omitempty`) — replaced with Go 1.24+'s
  `omitzero`, which does honor `time.Time.IsZero()`.

### CVE / misconfiguration scanning: Trivy

[`trivy`](https://trivy.dev) covers two purposes here, run at two different
layers:

1. **Image scanning** — `build.sh`'s `SCAN=true` runs `trivy image
   --severity HIGH,CRITICAL` against the built image, catching known CVEs
   in the OS/library layer the final `distroless/static-debian12` image
   ships (i.e. auditing the *runtime* artifact both modes deploy).
2. **Dependency and IaC scanning** — `trivy fs --scanners vuln,misconfig,secret .`
   run against the repo catches known CVEs in the Go module graph
   (`go.sum`), misconfigurations in the Kubernetes/Helm manifests under
   `deploy/` and `test/`, and accidentally committed secrets. This doesn't
   need a built image, so it's the layer to run in environments (like this
   one) where pulling `docker.io/library/golang` to build the image is
   blocked by egress policy.

Findings from the dependency/IaC pass, and what was done about each:

- **`deploy/deployment`'s pod spec had no pod-level `securityContext`**
  (`AVD-KSV-0118`, HIGH) — the container-level `securityContext` already
  set `runAsNonRoot`, but Trivy's check looks at the pod level too, and an
  empty `podSecurityContext: {}` doesn't communicate that intent for any
  future sidecar added to the pod. Fixed: `values.yaml` now defaults
  `podSecurityContext` to `runAsNonRoot: true` and
  `seccompProfile.type: RuntimeDefault`.
- **`deploy/deployment`'s default RBAC `Role` grants `create`/`update`/
  `delete` on `services`/`ingresses`** (`AVD-KSV-0056`, HIGH) — not fixed,
  by design: this chart's whole purpose is letting its ServiceAccount
  install arbitrary charts on a caller's behalf, and those charts create
  exactly these resource kinds (see `test/hello-world`). The
  [Security section](deploy/deployment/README.md#security) already tells
  operators to narrow `rbac.rules` to what they actually intend to serve;
  narrowing the shipped default would just break that stated use case for
  no security gain, since the true fix is per-deployment scoping, not a
  different one-size-fits-all default.
- **`golang.org/x/crypto@v0.54.0`, `GO-2026-5932`** (severity: `UNKNOWN`,
  an advisory rather than a scored CVE) — the advisory is about
  `golang.org/x/crypto/openpgp` being unmaintained and unsafe by design.
  Checked reachability with `go list -deps ./...`: this binary's build
  graph pulls in unrelated subpackages of that module (`bcrypt`, `blake2b`,
  `cast5`, etc., via transitive dependencies) but never `openpgp` itself —
  the actual OpenPGP implementation in this build graph is
  `github.com/ProtonMail/go-crypto/openpgp`, the maintained fork the
  advisory itself recommends. `go.sum` still pins the whole module
  (Go can't version subpackages independently), so `trivy fs` flags it at
  module granularity even though the vulnerable package isn't in the
  compiled binary. Nothing to fix; noted here rather than silenced, so a
  future dependency bump that actually starts using `x/crypto/openpgp`
  doesn't get missed by assuming this line item was already checked.

`trivy fs` also scanned `test/manifest.yaml` and `test/hello-world/`,
surfacing the same `AVD-KSV-0056`/`AVD-KSV-0118` classes — these are
pre-existing, intentionally broad (the RBAC there is explicitly scoped to
what that fixture's own chart needs, per the comment at the top of
`test/manifest.yaml`) and pose no more than a self-contained local test
harness ever did (the outcomes described in the manifest are unaffected).

## CI pipeline

[`.github/workflows/ci.yml`](.github/workflows/ci.yml) runs on `push` to
`main` or `dev` only. That single trigger covers all three cases it needs
to: a direct commit to `dev`, a PR merged into `dev` (merging creates a
push to the target branch), and a merge into `main` (same reasoning).
Adding `pull_request` as a second trigger alongside `push` would double-run
CI on every merge, since both events fire for it — so this is deliberately
not a `pull_request` + `push` combination.

Four independent jobs, each mirroring a check already established above:

- **`go`** — `go build`, `go vet`, `gofmt -l` (fails on any unformatted
  file), and `golangci-lint run` (see [Code quality](#code-quality)).
  golangci-lint is installed via `go install` rather than
  `golangci-lint-action`, so its binary is always built with the same Go
  toolchain `actions/setup-go` just installed — golangci-lint refuses to
  analyze code targeting a newer Go than it was built with, which is
  exactly the failure mode this session hit locally before switching to
  that install method.
- **`helm`** — `helm lint` and `helm template` against
  [`deploy/deployment`](deploy/deployment), plus `helm lint` against
  [`test/hello-world`](test/hello-world).
- **`trivy-fs`** — the dependency/IaC/secret scan from
  [CVE / misconfiguration scanning](#cve--misconfiguration-scanning-trivy),
  `--skip-dirs test` (those fixtures are intentionally out of scope, as
  covered there) and `exit-code: 1` so it actually gates merges. The
  already-triaged `AVD-KSV-0056` finding on `deploy/deployment`'s RBAC is
  suppressed via [`.trivyignore`](.trivyignore), which documents why
  inline rather than silently.
- **`image`** — builds the real image with `BUILDER=docker ./build.sh`
  and runs a Trivy *image* scan against it (the OS/library layer),
  `exit-code: 1`. `BUILDER=docker` is forced deliberately:
  `ubuntu-latest` runners ship both `docker` and `podman`, and `build.sh`
  prefers `podman` when present, which leaves the image in podman's local
  storage - `trivy-action`'s image scan checks `docker` first and doesn't
  find it there (its podman lookup needs a running podman *socket*, not
  just the CLI store), then fails every other lookup and finally tries
  (and fails) to pull `helm-sidecar:ci` from Docker Hub. First version of
  this job hit exactly that and reported as a CI failure with no real
  CVE behind it - fixed by making `BUILDER` overridable in `build.sh` and
  forcing it here. This is also the one job this repo's own sandboxed
  development sessions can't run end-to-end (pulling
  `docker.io/library/golang` is blocked by that environment's egress
  policy) - ordinary GitHub-hosted runners have no such restriction.

## Configuration (env vars)

| Var | Default | Purpose |
|---|---|---|
| `HELM_SIDECAR_MODE` | `sidecar` | `sidecar` binds `127.0.0.1`; `deployment` binds `0.0.0.0`. Any other value is a fatal startup error — there is no way to set an arbitrary bind address via env var. |
| `HELM_SIDECAR_PORT` | `8080` | Port the API listens on. |
| `KUBECONFIG` | unset (in-cluster config) | Only needed outside a cluster, e.g. local testing |
| `HELM_REGISTRY_CONFIG`, `HELM_REPOSITORY_CONFIG`, `HELM_REPOSITORY_CACHE` | Helm defaults under `$HOME` | Only relevant if you use `helm repo`-style chart refs or OCI registry login state; `$HOME` for the `nonroot` user is `/home/nonroot`, which is **not** guaranteed writable under `readOnlyRootFilesystem: true`. If you need repo/OCI caching under a read-only root, mount a small `emptyDir` there or point these vars at the shared volume. |

## Upgrading

**Helm SDK version**: bump `helm.sh/helm/v4` in [`go.mod`](go.mod), then
regenerate `go.sum`:

```bash
docker run --rm -v "$(pwd)":/app -w /app golang:1.26 go mod tidy
```

Helm v4 restructured its package layout significantly from v3 (e.g.
`pkg/chart` split into `pkg/chart` + `pkg/chart/v2`, releases returned as
`any` satisfied by `*release/v1.Release`, `WaitStrategy` must always be set
explicitly or the SDK errors). Before bumping across a minor version,
diff the actual SDK source for `pkg/action/{install,upgrade,uninstall,
status,lint}.go` at the target tag against what
[`internal/helmrunner/runner.go`](internal/helmrunner/runner.go) assumes —
don't assume the API is stable across releases the way `kubectl`/`helm`
CLI flags are.

**Go toolchain**: bump `go 1.x.x` in `go.mod` and `ARG GO_VERSION` in
`Containerfile` (or pass `GO_VERSION=1.x ./build.sh`) together. `go.mod`'s
`go` directive is a minimum-toolchain requirement — the builder image must
be at least that version.

**Base image**: `gcr.io/distroless/static-debian12:nonroot` is unversioned
beyond `debian12`; rebuilding picks up the latest published digest
automatically (glibc/CA-cert/timezone data patches). Pin to a specific
`@sha256:...` digest in the `Containerfile` if you need reproducible builds
instead of always-latest-patched.

**Verifying an upgrade**: `./build.sh` builds the real multi-stage image;
there's no separate "quick check" — a successful `docker build` already
runs `go build` against the pinned versions inside the container.

## License

[MIT](LICENSE).
