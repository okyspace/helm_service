#!/bin/sh
# Test harness for the helm-sidecar: copies the chart baked into this image
# at /app into the volume shared with the sidecar container (the sidecar
# can only see /shared-charts, not this container's private /app), then
# drives the sidecar's HTTP API to prove the whole pod wiring works.
set -eu

SIDECAR_URL="${HELM_SIDECAR_URL:-http://127.0.0.1:8080}"
CHART_NAME="hello-world"
CHART_SRC="/app/${CHART_NAME}"
SHARED_DIR="/shared-charts"
CHART_SHARED="${SHARED_DIR}/${CHART_NAME}"
RELEASE_NAME="${RELEASE_NAME:-hello}"
NAMESPACE="${NAMESPACE:-default}"
RUN_INSTALL="${RUN_INSTALL:-false}"

echo "Copying chart from ${CHART_SRC} into ${CHART_SHARED} (shared with sidecar)..."
mkdir -p "${SHARED_DIR}"
cp -r "${CHART_SRC}" "${CHART_SHARED}"

echo "Waiting for helm-sidecar at ${SIDECAR_URL}..."
until curl -sf "${SIDECAR_URL}/healthz" >/dev/null 2>&1; do
  sleep 1
done
echo "Sidecar is up."

echo "=== lint ==="
curl -sf -X POST -H 'Content-Type: application/json' \
  -d "{\"chart\":\"${CHART_SHARED}\"}" \
  "${SIDECAR_URL}/v1/lint"
echo

echo "=== template ==="
curl -sf -X POST -H 'Content-Type: application/json' \
  -d "{\"chart\":\"${CHART_SHARED}\",\"releaseName\":\"${RELEASE_NAME}\",\"namespace\":\"${NAMESPACE}\"}" \
  "${SIDECAR_URL}/v1/template"
echo

if [ "${RUN_INSTALL}" = "true" ]; then
  echo "=== install (RUN_INSTALL=true: this creates real resources in namespace ${NAMESPACE}) ==="
  curl -sf -X POST -H 'Content-Type: application/json' \
    -d "{\"chart\":\"${CHART_SHARED}\",\"releaseName\":\"${RELEASE_NAME}\",\"namespace\":\"${NAMESPACE}\",\"wait\":false}" \
    "${SIDECAR_URL}/v1/install"
  echo

  echo "=== status ==="
  curl -sf -X POST -H 'Content-Type: application/json' \
    -d "{\"releaseName\":\"${RELEASE_NAME}\",\"namespace\":\"${NAMESPACE}\"}" \
    "${SIDECAR_URL}/v1/status"
  echo
else
  echo "RUN_INSTALL is not 'true' -- skipping install/status (lint+template only, no cluster mutation)."
fi

echo "Done. Sleeping so the pod stays up for inspection (kubectl logs / kubectl exec)."
sleep infinity
