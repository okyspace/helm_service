# Sidecar mode

`HELM_SIDECAR_MODE=sidecar` (the default when the env var is unset). The
binary binds `127.0.0.1:8080` only, so it's reachable exclusively by other
containers sharing the same pod's network namespace - never by anything
outside the pod. See the [root README's "How it works"](../../README.md#how-it-works)
and [Security: TCP vs Unix socket](../../README.md#security-tcp-vs-unix-socket)
sections for the full design rationale.

There is no chart for this mode: the sidecar container is added directly to
whatever pod/deployment spec already carries your application, since it has
no lifecycle of its own separate from that workload.

## Requirements

1. Your application container and `helm-sidecar` in the same pod.
2. A ServiceAccount with RBAC covering whatever the charts you install
   actually need, plus read/write on `secrets` (Helm's default release
   storage driver) - see [`test/manifest.yaml`](../../test/manifest.yaml)
   for a worked example scoped to a real chart's resources.

## Example

[`examples/pod.yaml`](examples/pod.yaml) is the minimal pod fragment. Drop
it into your existing Deployment/StatefulSet/Pod spec's `containers:` list.

## Testing end to end

[`test/`](../../test) runs this exact pattern (app container + sidecar
sharing a pod) against the [`test/hello-world`](../../test/hello-world)
chart - see the root README's [Testing](../../README.md#testing) section.
