#!/usr/bin/env bash
# Smoke test: DLP on Skill (MCP) traffic and remote Skills, on a k3d cluster.
#
#   1. a Skill with a missing DLPPolicy is not deployed (fail closed)
#   2. with the policy, the MCP-mode proxy runs in the Skill Pod and tool
#      arguments are redacted before the MCP server sees them
#   3. the NetworkPolicy closes the server's own port
#   4. a remote (URL) Skill gets a gateway Pod that does the same
#
# Run from the repo root with a `make dev` cluster up:
#   bash scripts/smoke-skill-dlp.sh
set -euo pipefail
cd "$(dirname "$0")/.."

step() { printf '\n\033[1;34m== %s\033[0m\n' "$*"; }
fail() { printf '\n\033[1;31mFAIL: %s\033[0m\n' "$*"; exit 1; }
NS=smoke-sdlp
PIDS=()
cleanup() {
  for p in "${PIDS[@]:-}"; do [ -n "$p" ] && { kill "$p" 2>/dev/null || true; wait "$p" 2>/dev/null || true; }; done
  if [ -z "${KEEP:-}" ]; then
    kubectl delete namespace $NS --wait=false >/dev/null 2>&1 || true
  else
    echo "KEEP=1: leaving namespace $NS (kubectl delete ns $NS when done)"
  fi
}
trap cleanup EXIT

wait_ready() { # kind name
  local p=""
  for _ in $(seq 1 60); do
    p=$(kubectl -n $NS get "$1" "$2" -o jsonpath='{.status.phase}' 2>/dev/null || true)
    [ "$p" = "Ready" ] && return 0; sleep 3
  done
  fail "$1 $2 not Ready (phase=$p); kubectl -n $NS describe $1 $2"
}

# mcp_call PORT PATH-OF-SSE: opens the SSE stream, posts one tools/call with
# an AWS key in the arguments, prints the stream.
mcp_call() {
  local port=$1 sse=$2 out=/tmp/smoke-sdlp-$1.txt
  : > "$out"
  curl -sN "http://127.0.0.1:$port$sse" > "$out" 2>/dev/null &
  local pid=$!
  for _ in $(seq 1 20); do grep -q '^data: /' "$out" && break; sleep 0.5; done
  local ep; ep=$(grep -m1 '^data: ' "$out" | sed 's/^data: //' | tr -d '\r')
  [[ "$ep" == /* ]] || fail "endpoint event should be a path through the proxy, got: $ep"
  curl -s -o /dev/null -w 'POST %{http_code}\n' "http://127.0.0.1:$port$ep" -H 'Content-Type: application/json' \
    -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":"key AKIAIOSFODNN7EXAMPLE"}}}'
  sleep 2
  kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true
  cat "$out"
}

step "1. build CLI + images, install CRDs/RBAC, restart operator"
make build-cli image-dlp image-operator image-mcp-echo || fail "build"
kubectl apply -f operator/config/crd/bases/
# rbac.yaml lives in bairav-system, which manager.yaml creates: make it first
# (a fresh cluster, as in the nightly run, doesn't have it yet).
kubectl create namespace bairav-system --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f operator/config/rbac/
kubectl apply -f operator/config/manager/
kubectl -n bairav-system rollout restart deploy/bairav-operator
kubectl -n bairav-system rollout status deploy/bairav-operator --timeout=120s || fail "operator did not come up"
kubectl create namespace $NS --dry-run=client -o yaml | kubectl apply -f -

step "2. Skill with a missing policy is NOT deployed (fail closed)"
bin/bairav skill deploy bairav/mcp-echo:v1 --target k8s --namespace $NS --name echo --dlp-ref tools-guard --wait=false
# Poll, don't sleep: right after an operator restart the new leader may
# need a few seconds to take over.
MSG=""
for _ in $(seq 1 30); do
  MSG=$(kubectl -n $NS get skill echo -o jsonpath='{.status.conditions[?(@.type=="DLPEnforced")].message}' 2>/dev/null || true)
  [[ "$MSG" == *"missing DLPPolicy"* ]] && break
  sleep 2
done
kubectl -n $NS get deploy echo >/dev/null 2>&1 && fail "Deployment exists although the DLPPolicy is missing"
echo "condition: $MSG"
[[ "$MSG" == *"missing DLPPolicy"* ]] || fail "DLPEnforced=False not reported"
echo "✓ fail closed"

step "3. Create the policy; the proxy runs in the Skill Pod"
cat <<'EOF' | kubectl -n smoke-sdlp apply -f -
apiVersion: orch.bairavs.com/v1alpha1
kind: DLPPolicy
metadata: {name: tools-guard}
spec:
  packs: [global-safe]
  defaultAction: redact
  direction: both
EOF
wait_ready skill echo
CONTAINERS=$(kubectl -n $NS get pod -l orch.bairavs.com/skill=echo -o jsonpath='{.items[0].spec.containers[*].name}')
echo "containers: $CONTAINERS"
[[ "$CONTAINERS" == *skill*dlp* ]] || fail "dlp container not in the Skill Pod"
kubectl -n $NS get networkpolicy echo-dlp-only >/dev/null || fail "NetworkPolicy missing"
echo "✓ DLP in the Skill Pod"

step "4. Tool arguments are redacted before the MCP server sees them"
kubectl -n $NS port-forward svc/echo 18191:3000 >/dev/null 2>&1 &
PIDS+=($!); sleep 3
OUT=$(mcp_call 18191 /sse)
echo "$OUT"
[[ "$OUT" == *"[REDACTED:AWS_ACCESS_KEY]"* ]] || fail "no redaction in the tool result"
[[ "$OUT" == *AKIAIOSFODNN7EXAMPLE* ]] && fail "raw key reached the tool"
kubectl -n $NS logs deploy/echo -c dlp --tail=20 | grep -q AWS_ACCESS_KEY || fail "no hit in the Skill's DLP audit log"
echo "✓ redacted on the way to the tool"

step "5. The MCP server's own port is closed to the cluster"
POD_IP=$(kubectl -n $NS get pod -l orch.bairavs.com/skill=echo -o jsonpath='{.items[0].status.podIP}')
CODE=$(kubectl -n $NS run sdlp-probe --rm -i --restart=Never --image=curlimages/curl --quiet -- \
  sh -c "curl -s -m 5 -o /dev/null -w '%{http_code}' http://$POD_IP:3000/health || echo blocked" 2>/dev/null || true)
echo "direct to $POD_IP:3000 -> $CODE"
[[ "$CODE" == *blocked* || "$CODE" == *000* ]] || fail "MCP server reachable directly (does the CNI enforce NetworkPolicy?)"
echo "✓ only the DLP port is open"

step "6. Remote Skill: a gateway Pod in front of a URL"
bin/bairav skill deploy bairav/mcp-echo:v1 --target k8s --namespace $NS --name echo-plain
kubectl -n $NS create secret generic remote-mcp --from-literal=token=smoke-token --dry-run=client -o yaml | kubectl apply -f -
bin/bairav skill deploy "http://echo-plain.$NS.svc.cluster.local:3000/sse" --target k8s --namespace $NS \
  --name remote-echo --dlp-ref tools-guard --auth-secret remote-mcp:token
wait_ready skill remote-echo
EP=$(kubectl -n $NS get skill remote-echo -o jsonpath='{.status.endpoint}')
echo "endpoint: $EP"
[[ "$EP" == *"/sse" ]] || fail "remote endpoint should keep the server's path"
kubectl -n $NS get deploy remote-echo -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="BAIRAV_MCP_UPSTREAM_TOKEN")].valueFrom.secretKeyRef.name}' | grep -q remote-mcp \
  || fail "gateway does not read the token from the Secret"
kubectl -n $NS port-forward svc/remote-echo 18192:3000 >/dev/null 2>&1 &
PIDS+=($!); sleep 3
OUT=$(mcp_call 18192 /sse)
echo "$OUT"
[[ "$OUT" == *"[REDACTED:AWS_ACCESS_KEY]"* ]] || fail "remote gateway did not redact"
echo "✓ remote Skill through the gateway"

printf '\n\033[1;32mSKILL DLP + REMOTE SKILL SMOKE PASSED\033[0m\n'
