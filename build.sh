#!/bin/bash
set -e

cd "$(dirname "$0")"

APP_VERSION=$(cat VERSION)

REGISTRY=${REGISTRY:-""}
IMAGE_NAME=${IMAGE_NAME:-"helm-sidecar"}
TAG=${TAG:-"${APP_VERSION}"}
GO_VERSION=${GO_VERSION:-"1.26"}

FULL_IMAGE="${REGISTRY:+${REGISTRY}/}${IMAGE_NAME}:${TAG}"

BUILDER=${BUILDER:-""}
if [ -z "${BUILDER}" ]; then
    BUILDER="docker"
    if command -v podman &> /dev/null; then
        BUILDER="podman"
    fi
fi

echo "Building helm sidecar image: ${FULL_IMAGE} (builder: ${BUILDER}, go ${GO_VERSION}, app version ${APP_VERSION})"

${BUILDER} build \
    -f Containerfile \
    --build-arg GO_VERSION="${GO_VERSION}" \
    --build-arg VERSION="${APP_VERSION}" \
    -t "${FULL_IMAGE}" \
    .

echo "Built: ${FULL_IMAGE}"

if [ "${SCAN:-false}" = "true" ]; then
    if command -v trivy &> /dev/null; then
        echo "Scanning ${FULL_IMAGE} with trivy..."
        trivy image --severity HIGH,CRITICAL "${FULL_IMAGE}"
    else
        echo "SCAN=true but trivy is not installed; skipping scan." >&2
    fi
fi

if [ "${PUSH:-false}" = "true" ]; then
    ${BUILDER} push "${FULL_IMAGE}"
    echo "Pushed: ${FULL_IMAGE}"
fi
