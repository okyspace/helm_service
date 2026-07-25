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
   reach this Service, and `networkPolicy.egress` narrowed to what this
   pod itself actually needs to reach. It's `false` by default only so the
   chart installs cleanly on clusters without a NetworkPolicy controller -
   enable it wherever your CNI supports `NetworkPolicy`.

If you don't need multi-pod/multi-namespace reach and can instead run
helm-sidecar next to a single caller, prefer [sidecar mode](../sidecar/README.md):
NetworkPolicy has nothing to do there anyway - sidecar mode's loopback
traffic never crosses the CNI's dataplane, so a NetworkPolicy can neither
see nor restrict it (and NetworkPolicy selects whole Pods, not individual
containers within one - it can't express "only this container in the pod"
in the first place). Deployment mode's Service-routed traffic *does* cross
that dataplane, which is what makes NetworkPolicy meaningful here.

### Example: restrict ingress to a specific caller

Caller pod labelled `app.kubernetes.io/name: my-caller`, in a namespace
labelled `kubernetes.io/metadata.name: my-namespace` (both conditions must
hold - a single `from` peer with both selectors is AND, not OR):

```yaml
networkPolicy:
  enabled: true
  from:
    podSelector:
      matchLabels:
        app.kubernetes.io/name: my-caller
    namespaceSelector:
      matchLabels:
        kubernetes.io/metadata.name: my-namespace
```

Or the equivalent `--set`:

```bash
helm install helm-sidecar ./deploy/deployment \
  --set networkPolicy.enabled=true \
  --set networkPolicy.from.podSelector.matchLabels."app\.kubernetes\.io/name"=my-caller \
  --set-string networkPolicy.from.namespaceSelector.matchLabels."kubernetes\.io/metadata\.name"=my-namespace
```

To allow *any* pod in a trusted namespace regardless of its labels, set
only `namespaceSelector` and leave `podSelector: {}`.

### Example: narrow egress once you know your endpoints

The shipped default (DNS + HTTPS anywhere, port-restricted only) is
deliberately generic because this chart can't know your cluster's API
server address or your OCI registry/chart repo endpoints in advance.
Narrow it once you do:

```yaml
networkPolicy:
  enabled: true
  egress:
    - ports: [{ port: 53, protocol: UDP }, { port: 53, protocol: TCP }]
    # Kubernetes API server - note it's frequently on 6443, not 443, for
    # kubeadm/self-managed clusters, and on managed clusters (EKS/GKE/AKS)
    # it often isn't a selectable Pod at all, so podSelector-based rules
    # won't match it - use an ipBlock with the API server's real address
    # instead (kubectl get endpoints kubernetes -n default, or your cloud
    # provider's documented control-plane endpoint).
    - to:
        - ipBlock: { cidr: 10.0.0.5/32 }
      ports:
        - { port: 6443, protocol: TCP }
    # Your OCI registry / chart repo
    - to:
        - ipBlock: { cidr: 10.0.1.0/24 }
      ports:
        - { port: 443, protocol: TCP }
```

Set `networkPolicy.egress: []` instead for a fully locked-down install
with no egress at all (only useful if every chart you serve is already
cached locally and nothing needs live cluster-API or registry access,
which is not the common case).
