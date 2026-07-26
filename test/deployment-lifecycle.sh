#!/bin/bash
# Runs the full install/upgrade/list/history/rollback lifecycle (see
# lifecycle.sh) against helm-sidecar running in *deployment* mode: its own
# standalone Deployment + Service installed via the deploy/deployment
# Helm chart, reached here via `kubectl port-forward` against that
# Service.
#
# Requires: a reachable Kubernetes cluster (kubectl context already set),
# docker or podman, helm. Needs the image loaded into that cluster - see
# ../build.sh and, for minikube/kind, their image-load commands.
#
# Usage: ./deployment-lifecycle.sh
set -euo pipefail

cd "$(dirname "$0")"

NAMESPACE=${NAMESPACE:-"helm-sidecar-deployment-lifecycle-test"}
RELEASE_NAME=${RELEASE_NAME:-"helm-sidecar"}
LOCAL_PORT=${LOCAL_PORT:-18081}
SIDECAR_TAG=${SIDECAR_TAG:-"$(cat ../VERSION)"}

echo "Building helm-sidecar:${SIDECAR_TAG}..."
(cd .. && BUILDER=docker TAG="${SIDECAR_TAG}" ./build.sh)

echo "Serving test chart into ${NAMESPACE}..."
CHART_URL=$(./serve-chart.sh "$NAMESPACE")
echo "Chart URL: ${CHART_URL}"

echo "Installing helm-sidecar (deployment mode) via deploy/deployment chart..."
helm upgrade --install "$RELEASE_NAME" ../deploy/deployment \
  --namespace "$NAMESPACE" --create-namespace \
  --set image.tag="${SIDECAR_TAG}" \
  --set image.pullPolicy=IfNotPresent \
  --wait --timeout 120s

kubectl -n "$NAMESPACE" port-forward "svc/${RELEASE_NAME}" "${LOCAL_PORT}:8080" >/tmp/pf-deployment-lifecycle.log 2>&1 &
PF_PID=$!
trap 'kill "$PF_PID" 2>/dev/null || true' EXIT

echo "Running lifecycle test against http://127.0.0.1:${LOCAL_PORT} (deployment mode)..."
NAMESPACE="$NAMESPACE" ./lifecycle.sh "http://127.0.0.1:${LOCAL_PORT}" "$CHART_URL"

echo
echo "Teardown: helm -n ${NAMESPACE} uninstall ${RELEASE_NAME} && kubectl delete namespace ${NAMESPACE}"
