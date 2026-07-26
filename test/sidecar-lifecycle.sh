#!/bin/bash
# Runs the full install/upgrade/list/history/rollback lifecycle (see
# lifecycle.sh) against helm-sidecar running in *sidecar* mode: a bare pod
# binding 127.0.0.1:8080, reached here via `kubectl port-forward` against
# the pod (which works even for a loopback-only bind - port-forward enters
# the pod's network namespace, it doesn't NAT through an external
# interface the way a Service or `docker run -p` would).
#
# This is a separate, minimal pod from test/manifest.yaml's app+sidecar
# pair (which exists to prove the shared-pod wiring itself, via lint and
# template only) - this one is purely for exercising the mutating lifecycle
# end to end.
#
# Requires: a reachable Kubernetes cluster (kubectl context already set),
# docker or podman, helm. Needs the image loaded into that cluster - see
# ../build.sh and, for minikube/kind, their image-load commands.
#
# Usage: ./sidecar-lifecycle.sh
set -euo pipefail

cd "$(dirname "$0")"

NAMESPACE=${NAMESPACE:-"helm-sidecar-sidecar-lifecycle-test"}
POD_NAME=${POD_NAME:-"helm-sidecar-lifecycle-test"}
LOCAL_PORT=${LOCAL_PORT:-18080}
SIDECAR_TAG=${SIDECAR_TAG:-"$(cat ../VERSION)"}

echo "Building helm-sidecar:${SIDECAR_TAG}..."
(cd .. && BUILDER=docker TAG="${SIDECAR_TAG}" ./build.sh)

echo "Serving test chart into ${NAMESPACE}..."
CHART_URL=$(./serve-chart.sh "$NAMESPACE")
echo "Chart URL: ${CHART_URL}"

echo "Starting helm-sidecar pod (sidecar mode) in ${NAMESPACE}..."
# RBAC scoped to exactly what test/hello-world's resources need, plus
# Helm's own release-storage Secrets - same scope test/manifest.yaml uses
# for the same reason. A bare pod like this one otherwise runs as its
# namespace's "default" ServiceAccount, which has no permissions at all in
# most clusters.
kubectl -n "$NAMESPACE" apply -f - <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ${POD_NAME}-sa
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: ${POD_NAME}-role
rules:
  - apiGroups: [""]
    resources: ["secrets", "configmaps", "services", "persistentvolumeclaims", "serviceaccounts"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: ["apps"]
    resources: ["deployments"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: ["networking.k8s.io"]
    resources: ["ingresses"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: ${POD_NAME}-rb
subjects:
  - kind: ServiceAccount
    name: ${POD_NAME}-sa
roleRef:
  kind: Role
  name: ${POD_NAME}-role
  apiGroup: rbac.authorization.k8s.io
---
apiVersion: v1
kind: Pod
metadata:
  name: ${POD_NAME}
  labels: { app: ${POD_NAME} }
spec:
  serviceAccountName: ${POD_NAME}-sa
  restartPolicy: Never
  containers:
    - name: helm-sidecar
      image: helm-sidecar:${SIDECAR_TAG}
      imagePullPolicy: IfNotPresent
      ports:
        - { containerPort: 8080 }
      securityContext:
        runAsNonRoot: true
        readOnlyRootFilesystem: true
        allowPrivilegeEscalation: false
EOF
kubectl -n "$NAMESPACE" wait --for=condition=Ready "pod/${POD_NAME}" --timeout=60s

kubectl -n "$NAMESPACE" port-forward "pod/${POD_NAME}" "${LOCAL_PORT}:8080" >/tmp/pf-sidecar-lifecycle.log 2>&1 &
PF_PID=$!
trap 'kill "$PF_PID" 2>/dev/null || true' EXIT

echo "Running lifecycle test against http://127.0.0.1:${LOCAL_PORT} (sidecar mode)..."
NAMESPACE="$NAMESPACE" ./lifecycle.sh "http://127.0.0.1:${LOCAL_PORT}" "$CHART_URL"

echo
echo "Teardown: kubectl delete namespace ${NAMESPACE}"
