# Helm Sidecar

A Kubernetes sidecar that gives another container in the same pod the
ability to run Helm install/upgrade/uninstall/status/template/lint —
**without a shell, a `helm` binary, or a subprocess anywhere in the path**.

It links the [Helm v4 Go SDK](https://pkg.go.dev/helm.sh/helm/v4/pkg/action)
directly and exposes it as an HTTP/JSON API over loopback TCP
(`127.0.0.1:8080`). The calling container talks to that port; nothing on
either side ever builds a command line or execs a process, so there is no
command-injection surface between the two containers.

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

```
┌─────────────────────── pod (one network namespace) ───────────────┐
│  ┌─────────────────┐                    ┌────────────────────┐    │
│  │  your container  │  HTTP/JSON         │   helm-sidecar      │    │
│  │  (any language)  │──▶ 127.0.0.1:8080 ▶│   (this image)      │    │
│  └─────────────────┘                    └────────────────────┘    │
└─────────────────────────────────────────────────────────────────┘
```

- `internal/helmrunner` wraps `action.Install` / `action.Upgrade` /
  `action.Uninstall` / `action.Status` / `action.NewInstall` (dry-run, for
  `template`) / `action.NewLint`. Every request builds its own
  `action.Configuration` and `genericclioptions.RESTClientGetter`, so
  concurrent requests targeting different namespaces can't race on shared
  state.
- `internal/api` is a plain `net/http` server (stdlib only — no gRPC, no
  third-party HTTP framework) bound to `127.0.0.1:8080` — never `0.0.0.0`;
  see [Security: TCP vs Unix socket](#security-tcp-vs-unix-socket) below.
  Requests are strict JSON (`DisallowUnknownFields`), capped at 1 MiB,
  release/namespace names are validated against the Kubernetes DNS-1123
  pattern before touching the SDK.
- Kubernetes auth is whatever the pod's ServiceAccount provides (in-cluster
  config, the standard client-go fallback) unless `KUBECONFIG` is set.

## Security: TCP vs Unix socket

This started as a Unix domain socket on a shared `emptyDir`, then was
switched to loopback TCP. Both are pod-local only — nothing outside the
pod can reach either — but the access-control model differs:

- **Unix socket** (the original design): reachable only by containers that
  explicitly mount the volume holding the socket file, and further gated
  by the socket's file permission bits (`0660` + matching `runAsGroup`/
  `fsGroup`). A container with no reason to talk to Helm, and no volume
  mount for it, structurally cannot reach it.
- **Loopback TCP** (current): every container in the pod shares one
  network namespace, so `127.0.0.1:8080` is reachable by *any* container in
  the pod — including anything added later (a service-mesh sidecar, a
  `kubectl debug` ephemeral container) — with no extra configuration. In
  exchange, there's no shared-volume/permission-bit plumbing to get wrong,
  which was the main friction of the socket approach outside Kubernetes
  (plain `docker run` has no `fsGroup` equivalent).

Both approaches are inherently safe against exposure *outside the pod*:
the socket has no network representation at all, and this code binds TCP
to `127.0.0.1` specifically (hardcoded as the bind address in
[`cmd/helmsidecar/main.go`](cmd/helmsidecar/main.go) — only the port is
configurable) so it can't be widened to `0.0.0.0` by a stray env var.

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

The sidecar needs:

1. Any container in the pod can reach `127.0.0.1:8080` as soon as the
   sidecar is up.
2. **A ServiceAccount with RBAC** in whatever namespaces it will manage —
   at minimum, read/write on `secrets` (Helm's default release storage
   driver) plus whatever permissions the charts it installs actually need.

Minimal pod fragment:

```yaml
spec:
  serviceAccountName: helm-sidecar-sa
  containers:
    - name: app
      image: your-app:latest
      env:
        - { name: HELM_SIDECAR_URL, value: "http://127.0.0.1:8080" }
    - name: helm-sidecar
      image: helm-sidecar:latest
      ports:
        - { containerPort: 8080 }
      securityContext:
        runAsNonRoot: true
        readOnlyRootFilesystem: true
        allowPrivilegeEscalation: false
```

## Using the API

All endpoints are POST with a JSON body, except `GET /healthz`. Examples
below assume the calling container reaches the sidecar at
`http://127.0.0.1:8080` (the pod-shared loopback address).

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

## Configuration (env vars)

| Var | Default | Purpose |
|---|---|---|
| `HELM_SIDECAR_PORT` | `8080` | Port the API listens on. The bind address is always `127.0.0.1`, hardcoded — not configurable via env var. |
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
