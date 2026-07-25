# Helm Sidecar

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
./build.sh                       # -> helm-sidecar:latest
REGISTRY=harbor.local/ainexushub TAG=v1.0.0 PUSH=true ./build.sh
SCAN=true ./build.sh             # also runs `trivy image` if installed
```

Env vars `build.sh` reads: `REGISTRY`, `IMAGE_NAME` (default
`helm-sidecar`), `TAG` (default `latest`), `GO_VERSION` (default `1.26`),
`PUSH`, `SCAN`.

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

All endpoints are POST with a JSON body, except `GET /healthz`. The API
itself is identical in both modes; only the address differs. Examples
below assume sidecar mode, i.e. the calling container reaches
helm-sidecar at `http://127.0.0.1:8080` (the pod-shared loopback address).
In deployment mode, replace that with the Service's cluster-DNS name, e.g.
`http://helm-sidecar.helm-sidecar.svc.cluster.local:8080`.

```bash
# health check
curl http://127.0.0.1:8080/healthz

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
