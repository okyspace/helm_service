#!/bin/bash
# Packages test/hello-world and serves it over plain HTTP from a throwaway
# pod inside the cluster, then prints the resulting chart URL on stdout.
#
# Why: the helm-sidecar image is distroless (no shell, nothing to exec
# into or volume-mount tricks with), in both modes. Rather than special-
# casing "how do I get a chart onto this filesystem", this uses the exact
# mechanism the sidecar already supports for any real-world chart: an
# http(s) URL to a packaged .tgz (see ChartRef in
# internal/helmrunner/runner.go). Sidecar mode's existing pod-local test
# (test/manifest.yaml) uses a shared emptyDir instead, since it has an app
# container to copy the chart from - this script is for the two new
# lifecycle tests, which don't have one.
#
# Usage: serve-chart.sh <namespace>
# Prints: http://chart-server.<namespace>.svc.cluster.local:8000/<chart>.tgz
set -euo pipefail

cd "$(dirname "$0")"

NAMESPACE=${1:?"usage: $0 <namespace>"}
PKG_DIR=$(mktemp -d)
trap 'rm -rf "$PKG_DIR"' EXIT

helm package hello-world -d "$PKG_DIR" >&2
CHART_TGZ=$(basename "$PKG_DIR"/*.tgz)

kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f - >&2

kubectl -n "$NAMESPACE" delete configmap chart-pkg --ignore-not-found >&2
kubectl -n "$NAMESPACE" create configmap chart-pkg --from-file="$PKG_DIR" >&2

kubectl -n "$NAMESPACE" apply -f - >&2 <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: chart-server
spec:
  replicas: 1
  selector:
    matchLabels: { app: chart-server }
  template:
    metadata:
      labels: { app: chart-server }
    spec:
      containers:
        - name: chart-server
          image: docker.io/library/python:3-alpine
          command: ["python3", "-m", "http.server", "8000", "--directory", "/charts"]
          volumeMounts:
            - { name: charts, mountPath: /charts, readOnly: true }
      volumes:
        - name: charts
          configMap: { name: chart-pkg }
---
apiVersion: v1
kind: Service
metadata:
  name: chart-server
spec:
  selector: { app: chart-server }
  ports:
    - { port: 8000, targetPort: 8000 }
EOF

kubectl -n "$NAMESPACE" rollout status deployment/chart-server --timeout=60s >&2

echo "http://chart-server.${NAMESPACE}.svc.cluster.local:8000/${CHART_TGZ}"
