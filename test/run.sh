#!/bin/bash
set -e

cd "$(dirname "$0")"

APP_IMAGE=${APP_IMAGE:-"helm-sidecar-test-app:latest"}
SIDECAR_IMAGE=${SIDECAR_IMAGE:-"helm-sidecar:latest"}
NAMESPACE=${NAMESPACE:-"helm-sidecar-test"}
DEPLOYMENT=${DEPLOYMENT:-"hello-world-test"}

BUILDER="docker"
if command -v podman &> /dev/null; then
    BUILDER="podman"
fi

echo "Building test app image: ${APP_IMAGE} (builder: ${BUILDER})"
${BUILDER} build -f app/Containerfile -t "${APP_IMAGE}" .

if ! ${BUILDER} image inspect "${SIDECAR_IMAGE}" >/dev/null 2>&1; then
    echo "${SIDECAR_IMAGE} not found locally, building it via ../build.sh"
    (cd .. && TAG="${SIDECAR_IMAGE#*:}" ./build.sh)
fi

MINIKUBE_PROFILE=${MINIKUBE_PROFILE:-$(kubectl config current-context 2>/dev/null)}

if command -v minikube &> /dev/null && minikube status -p "${MINIKUBE_PROFILE}" &> /dev/null; then
    echo "Loading images into minikube profile '${MINIKUBE_PROFILE}'..."
    minikube image load "${APP_IMAGE}" -p "${MINIKUBE_PROFILE}"
    minikube image load "${SIDECAR_IMAGE}" -p "${MINIKUBE_PROFILE}"
else
    echo "minikube not running/detected (checked profile '${MINIKUBE_PROFILE}'): assuming your" >&2
    echo "current kubectl context can already pull these images (e.g. 'kind load docker-image'," >&2
    echo "a reachable registry, etc)." >&2
fi

echo "Applying manifest.yaml..."
kubectl apply -f manifest.yaml

echo "Waiting for deployment/${DEPLOYMENT} to become ready in namespace ${NAMESPACE}..."
kubectl -n "${NAMESPACE}" rollout status "deployment/${DEPLOYMENT}" --timeout=180s

echo
echo "App container logs (lint/template/install output):"
kubectl -n "${NAMESPACE}" logs "deployment/${DEPLOYMENT}" -c app --tail=200

echo
echo "Teardown: kubectl delete -f manifest.yaml"
