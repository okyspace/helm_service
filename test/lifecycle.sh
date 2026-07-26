#!/bin/bash
# Exercises a full Helm release lifecycle through the sidecar's HTTP API:
# install -> list -> upgrade (with override.yaml's values) -> history ->
# rollback -> status. The API is identical in both modes, so this one
# script drives either - only the base URL and chart ref differ, which
# sidecar-lifecycle.sh and deployment-lifecycle.sh work out per mode
# before calling this.
#
# override.yaml is written as JSON-compatible YAML (a valid JSON document
# is also valid YAML) specifically so it can be embedded into a request
# body with `jq` directly, with no YAML-to-JSON conversion step/dependency.
#
# Usage: lifecycle.sh <sidecar-base-url> <chart-ref>
# Example: lifecycle.sh http://127.0.0.1:18080 http://chart-server:8000/hello-world-0.1.0.tgz
set -euo pipefail

cd "$(dirname "$0")"

BASE_URL=${1:?"usage: $0 <sidecar-base-url> <chart-ref>"}
CHART_REF=${2:?"usage: $0 <sidecar-base-url> <chart-ref>"}
RELEASE_NAME=${RELEASE_NAME:-"lifecycle-test"}
NAMESPACE=${NAMESPACE:-"default"}
OVERRIDE_VALUES=$(cat override.yaml)

post() {
    local path="$1" body="$2"
    curl -sS -w '\n%{http_code}' -X POST -H 'Content-Type: application/json' -d "$body" "${BASE_URL}${path}"
}

# Splits the curl output (body + trailing status code line) apart, and
# fails loudly on a non-2xx instead of letting a bad response cascade into
# a confusing jq parse error two steps later.
check_2xx() {
    local label="$1" response="$2"
    local code body
    code=$(echo "$response" | tail -1)
    body=$(echo "$response" | sed '$d')
    if [ "$code" -lt 200 ] || [ "$code" -ge 300 ]; then
        echo "FAIL: $label returned HTTP $code: $body" >&2
        exit 1
    fi
    echo "$body"
}

echo "Waiting for helm-sidecar at ${BASE_URL}..."
until curl -sf "${BASE_URL}/healthz" >/dev/null 2>&1; do sleep 1; done

echo "=== install ==="
resp=$(post /v1/install "$(jq -n --arg chart "$CHART_REF" --arg name "$RELEASE_NAME" --arg ns "$NAMESPACE" \
    '{chart:$chart, releaseName:$name, namespace:$ns, createNamespace:true, wait:true}')")
install_out=$(check_2xx "install" "$resp")
echo "$install_out" | jq .
rev1=$(echo "$install_out" | jq -r '.revision')
[ "$rev1" = "1" ] || { echo "FAIL: expected revision 1 after install, got $rev1" >&2; exit 1; }

echo "=== list ==="
resp=$(post /v1/list "$(jq -n --arg ns "$NAMESPACE" '{namespace:$ns}')")
list_out=$(check_2xx "list" "$resp")
echo "$list_out" | jq .
echo "$list_out" | jq -e --arg name "$RELEASE_NAME" 'any(.[]; .name == $name)' >/dev/null \
    || { echo "FAIL: $RELEASE_NAME not present in list" >&2; exit 1; }

echo "=== upgrade (with override.yaml) ==="
resp=$(post /v1/upgrade "$(jq -n --arg chart "$CHART_REF" --arg name "$RELEASE_NAME" --arg ns "$NAMESPACE" \
    --argjson values "$OVERRIDE_VALUES" \
    '{chart:$chart, releaseName:$name, namespace:$ns, wait:true, values:$values}')")
upgrade_out=$(check_2xx "upgrade" "$resp")
echo "$upgrade_out" | jq .
rev2=$(echo "$upgrade_out" | jq -r '.revision')
[ "$rev2" = "2" ] || { echo "FAIL: expected revision 2 after upgrade, got $rev2" >&2; exit 1; }
echo "$upgrade_out" | jq -e '.manifest | contains("replicas: 2")' >/dev/null \
    || { echo "FAIL: upgraded manifest doesn't reflect override.yaml's replicaCount: 2" >&2; exit 1; }

echo "=== history ==="
resp=$(post /v1/history "$(jq -n --arg name "$RELEASE_NAME" --arg ns "$NAMESPACE" '{releaseName:$name, namespace:$ns}')")
history_out=$(check_2xx "history" "$resp")
echo "$history_out" | jq .
history_count=$(echo "$history_out" | jq 'length')
[ "$history_count" -ge 2 ] || { echo "FAIL: expected at least 2 revisions in history, got $history_count" >&2; exit 1; }

echo "=== rollback (to revision 1) ==="
resp=$(post /v1/rollback "$(jq -n --arg name "$RELEASE_NAME" --arg ns "$NAMESPACE" '{releaseName:$name, namespace:$ns, revision:1, wait:true}')")
rollback_out=$(check_2xx "rollback" "$resp")
echo "$rollback_out" | jq .

echo "=== status (confirm the rollback landed) ==="
resp=$(post /v1/status "$(jq -n --arg name "$RELEASE_NAME" --arg ns "$NAMESPACE" '{releaseName:$name, namespace:$ns}')")
status_out=$(check_2xx "status" "$resp")
echo "$status_out" | jq .
# Helm's rollback creates a new revision restoring the target's content
# rather than rewriting history in place, so this is revision 3, not 1.
rev3=$(echo "$status_out" | jq -r '.revision')
[ "$rev3" = "3" ] || { echo "FAIL: expected revision 3 after rollback (helm rollback creates a new revision), got $rev3" >&2; exit 1; }
echo "$status_out" | jq -e '.manifest | contains("replicas: 1")' >/dev/null \
    || { echo "FAIL: post-rollback manifest doesn't show the original replicaCount: 1" >&2; exit 1; }

echo
echo "All lifecycle checks passed: install -> list -> upgrade -> history -> rollback -> status"
