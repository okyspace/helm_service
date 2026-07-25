# Deployment mode

`HELM_SIDECAR_MODE=deployment`. The binary binds `0.0.0.0:8080` instead of
loopback, because it now runs as its own pod with no application
container to share a network namespace with - a Kubernetes `Service` must
be able to route to it from elsewhere in the cluster. Everything else
(the HTTP/JSON API, the Helm SDK usage, the security posture around never
shelling out) is identical to sidecar mode - see the [root README](../../README.md).

This directory **is** the Helm chart (`Chart.yaml` at its root) that
installs helm-sidecar this way: a `Deployment`, a `ClusterIP` `Service`, a
`ServiceAccount`, namespaced `Role`/`RoleBinding` (or cluster-wide, see
below), and an optional `NetworkPolicy`.

## Install

```bash
helm install helm-sidecar ./deploy/deployment \
  --namespace helm-sidecar --create-namespace \
  --set image.repository=harbor.local/ainexushub/helm-sidecar \
  --set image.tag=v1.0.0
```

Same image as sidecar mode (see [`../../build.sh`](../../build.sh) /
[`../../Containerfile`](../../Containerfile)) - only the `HELM_SIDECAR_MODE`
env var the chart sets differs.

## Values

See [`values.yaml`](values.yaml) for the full set, notably:

- `rbac.rules` - what the ServiceAccount can do. Defaults cover Helm's own
  release-storage `Secrets` plus the resource kinds `test/hello-world`
  uses; narrow or widen to match the charts you intend this instance to
  serve.
- `rbac.clusterWide` - `false` (namespaced `Role`) by default; set `true`
  for a single instance managing releases across multiple namespaces
  (`ClusterRole`/`ClusterRoleBinding`).
- `networkPolicy.enabled` - see Security below.

## Security

Sidecar mode is reachable only by containers in the same pod, by
construction - there's no separate access-control step to configure.
Deployment mode gives up that structural guarantee: the Service is
reachable by any workload in the cluster that can route to it, and the
HTTP API itself does not authenticate callers. Two things bound the blast
radius, and both are your responsibility to configure per environment:

1. **`rbac.rules`** - the ServiceAccount's permissions are the ceiling on
   what any caller can do through this instance. Scope them to exactly the
   charts you intend to serve, not a superset "just in case".
2. **`networkPolicy.enabled=true`**, with `networkPolicy.from.podSelector`
   and/or `.namespaceSelector` set to the specific callers that should
   reach this Service. It's `false` by default only so the chart installs
   cleanly on clusters without a NetworkPolicy controller - enable it
   wherever your CNI supports `NetworkPolicy`.

If you don't need multi-pod/multi-namespace reach and can instead run
helm-sidecar next to a single caller, prefer [sidecar mode](../sidecar/README.md):
it has no equivalent network-exposure question to answer.
