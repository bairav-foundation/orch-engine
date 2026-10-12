#!/usr/bin/env bash
# Smoke test: model-side DLP (Model spec.dlpRef) on a k3d cluster.
#
# Checks that with --dlp-ref the DLP proxy runs inside the Model Pod, the
# serving container is loopback-only, traffic to the Service is redacted,
# and nothing in the cluster can reach llama-server directly.
#
# Run from the repo root with a `make dev` cluster up:
#   bash scripts/smoke-model-dlp.sh
set -euo pipefail
cd "$(dirname "$0")/.."

step() { printf '\n\033[1;34m== %s\033[0m\n' "$*"; }
fail() { printf '\n\033[1;31mFAIL: %s\033[0m\n' "$*"; exit 1; }
NS=smoke-mdlp
MODEL=mdlp-qwen
REPO=Qwen/Qwen2.5-0.5B-Instruct-GGUF
PF=""
# Always clean up, pass or fail. KEEP=1 leaves the namespace for debugging.
cleanup() {
  if [ -n "$PF" ]; then kill "$PF" 2>/dev/null || true; wait "$PF" 2>/dev/null || true; fi
  if [ -z "${KEEP:-}" ]; then
    kubectl delete namespace $NS --wait=false >/dev/null 2>&1 || true
  else
    echo "KEEP=1: leaving namespace $NS (kubectl delete ns $NS when done)"
  fi
}
trap cleanup EXIT

step "1. build CLI + images, install CRDs/RBAC, restart operator"
make build-cli image-serving image-dlp image-operator || fail "build"
kubectl apply -f operator/config/crd/bases/
kubectl apply -f operator/config/rbac/
kubectl apply -f operator/config/manager/
kubectl -n bairav-system rollout restart deploy/bairav-operator
kubectl -n bairav-system rollout status deploy/bairav-operator --timeout=120s || fail "operator did not come up"

step "2. Model with a missing policy is NOT deployed (fail closed)"
kubectl create namespace $NS --dry-run=client -o yaml | kubectl apply -f -
kubectl -n $NS delete model $MODEL --ignore-not-found --wait=true
kubectl -n $NS delete dlp mdlp-policy --ignore-not-found
bin/bairav model deploy $REPO --target k8s --namespace $NS --name $MODEL --dlp-ref mdlp-policy --wait=false
sleep 8
kubectl -n $NS get deploy $MODEL >/dev/null 2>&1 && fail "Deployment exists although the DLPPolicy is missing"
MSG=$(kubectl -n $NS get model $MODEL -o jsonpath='{.status.conditions[?(@.type=="DLPEnforced")].message}')
echo "condition: $MSG"
[[ "$MSG" == *"missing DLPPolicy"* ]] || fail "DLPEnforced=False condition not reported"
echo "✓ fail closed"

step "3. Create the policy; the Model rolls out with DLP in its Pod"
cat <<'EOF' | kubectl -n smoke-mdlp apply -f -
apiVersion: orch.bairavs.com/v1alpha1
kind: DLPPolicy
metadata: {name: mdlp-policy}
spec:
  packs: [global-safe]
  customRules:
    - {label: TICKET_ID, regex: 'TKT-\d{6}'}
  defaultAction: redact
  direction: both
EOF
for i in $(seq 1 90); do
  P=$(kubectl -n $NS get model $MODEL -o jsonpath='{.status.phase}' 2>/dev/null || true)
  [ "$P" = "Ready" ] && break; sleep 5
done
[ "$P" = "Ready" ] || fail "Model not Ready (phase=$P); kubectl -n $NS describe model $MODEL"
CONTAINERS=$(kubectl -n $NS get pod -l orch.bairavs.com/model=$MODEL -o jsonpath='{.items[0].spec.containers[*].name}')
echo "containers: $CONTAINERS"
[[ "$CONTAINERS" == *serving*dlp* ]] || fail "dlp container not in the Model Pod"
[ "$(kubectl -n $NS get model $MODEL -o jsonpath='{.status.conditions[?(@.type=="DLPEnforced")].status}')" = "True" ] || fail "DLPEnforced not True"
echo "✓ DLP in the Model Pod"

step "4. Traffic through the Service is redacted"
kubectl -n $NS port-forward svc/$MODEL 18181:8080 >/dev/null 2>&1 &
PF=$!; sleep 3
curl -s http://127.0.0.1:18181/v1/chat/completions -H 'Content-Type: application/json' \
  -d '{"messages":[{"role":"user","content":"Repeat exactly: my aadhaar is 2345 6789 0124 and ticket TKT-123456"}],"max_tokens":40}' | tee /tmp/smoke-mdlp.json
echo
kubectl -n $NS logs deploy/$MODEL -c dlp --tail=3
kubectl -n $NS logs deploy/$MODEL -c dlp --tail=20 | grep -q AADHAAR || fail "no AADHAAR hit in the model's DLP audit log"
grep -q "2345 6789 0124" /tmp/smoke-mdlp.json && fail "raw Aadhaar came back" || true
echo "✓ redacted at the model"

step "5. No direct path to llama-server"
POD_IP=$(kubectl -n $NS get pod -l orch.bairavs.com/model=$MODEL -o jsonpath='{.items[0].status.podIP}')
OUT=$(kubectl -n $NS run mdlp-probe --rm -i --restart=Never --image=curlimages/curl --quiet -- \
  sh -c "curl -s -m 5 -o /dev/null -w '%{http_code}' http://$POD_IP:18080/health || echo refused" 2>/dev/null || true)
echo "direct to $POD_IP:18080 -> $OUT"
[[ "$OUT" == *refused* || "$OUT" == *000* ]] || fail "llama-server reachable directly on the Pod IP"
echo "✓ serving container is loopback-only"

step "6. Redeploy without --dlp-ref keeps the policy"
# Capture first: `cmd | grep -q` under pipefail fails when grep exits early
# and the CLI gets SIGPIPE on its next line.
OUT=$(bin/bairav model deploy $REPO --target k8s --namespace $NS --name $MODEL --wait=false 2>&1) || fail "redeploy failed: $OUT"
echo "$OUT" | tail -4
[[ "$OUT" == *"Keeping DLPPolicy mdlp-policy"* ]] || fail "redeploy didn't say it kept the policy"
[ "$(kubectl -n $NS get model $MODEL -o jsonpath='{.spec.dlpRef.name}')" = "mdlp-policy" ] || fail "dlpRef gone"
echo "✓ dlpRef preserved"

printf '\n\033[1;32mMODEL-SIDE DLP SMOKE PASSED\033[0m\n'
