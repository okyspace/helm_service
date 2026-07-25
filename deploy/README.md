# Deploying helm-sidecar

helm-sidecar runs in one of two modes, both built from the same image
(`../Containerfile`, `../build.sh`) and switched by the `HELM_SIDECAR_MODE`
env var. Everything specific to *how* you run it lives in one of these two
directories; everything the modes share (Go source, the container image,
the HTTP/JSON API itself) lives at the repo root - see the
[root README](../README.md#folder-structure).

| | [`sidecar/`](sidecar/README.md) | [`deployment/`](deployment/README.md) |
|---|---|---|
| `HELM_SIDECAR_MODE` | `sidecar` (default) | `deployment` |
| Runs as | a container in *your* pod | its own standalone Deployment |
| Bind address | `127.0.0.1` (loopback only) | `0.0.0.0` (fronted by a Service) |
| Reachable by | containers in the same pod only | anything that can route to the Service |
| Ships as | an example pod fragment | a Helm chart |
