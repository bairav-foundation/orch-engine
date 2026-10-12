#!/usr/bin/env bash
# Smoke test for Days 17-19: visualize, DLPPolicy CRD, Agent DLP sidecar,
# customRules projection, response + streaming redaction.
#
# Run from the orch-engine root on a machine with Go + docker + k3d + a
# running `make dev` cluster:
#   bash scripts/smoke-day18-19.sh
#
# Stops at the first failure and says which step broke.
set -euo pipefail
cd "$(dirname "$0")/.."

step() { printf '\n\033[1;34m== %s\033[0m\n' "$*"; }
fail() { printf '\n\033[1;31mFAIL: %s\033[0m\n' "$*"; exit 1; }

step "0. make fmt (make rebuild runs scripts/ci.sh, whose gofmt check fails on unformatted code)"
UNFORMATTED=$( (cd cli && gofmt -l .; cd ../operator && gofmt -l .) || true)
if [ -n "$UNFORMATTED" ]; then
  echo "gofmt will rewrite:"; echo "$UNFORMATTED" | sed 's/^/  /'
fi
make fmt || fail "make fmt"

step "1. go vet + unit tests (cli + operator)"
(cd cli && go vet ./... && go test ./...) || fail "cli vet/test"
(cd operator && go vet ./... && go test ./...) || fail "operator vet/test"

step "2. rebuild CLI + images (operator, dlp sidecar)"
make rebuild || fail "make rebuild"
make image-operator || fail "image-operator"
make image-dlp || fail "image-dlp (sidecar needs --direction + BAIRAV_DLP_PACK support)"

step "3. apply CRDs + RBAC, restart operator"
kubectl apply -f operator/config/crd/bases/
kubectl apply -f operator/config/rbac/
kubectl -n bairav-system rollout restart deploy/bairav-operator
kubectl -n bairav-system rollout status deploy/bairav-operator --timeout=120s || fail "operator did not come up (check RBAC / logs)"

step "4. DLPPolicy validation: good policy → Ready, bad policy → Invalid"
cat <<'EOF' | kubectl apply -f -
apiVersion: orch.bairavs.com/v1alpha1
kind: DLPPolicy
metadata: {name: smoke-good}
spec:
  packs: [global-safe]
  customRules:
    - {label: TICKET_ID, regex: 'TKT-\d{6}'}
  defaultAction: redact
  direction: both
---
apiVersion: orch.bairavs.com/v1alpha1
kind: DLPPolicy
metadata: {name: smoke-bad}
spec:
  customRules:
    - {label: EMAIL, regex: '(unclosed'}
EOF
sleep 5
kubectl get dlp
[ "$(kubectl get dlp smoke-good -o jsonpath='{.status.phase}')" = "Ready" ] || fail "smoke-good not Ready"
[ "$(kubectl get dlp smoke-bad -o jsonpath='{.status.phase}')" = "Invalid" ] || fail "smoke-bad not Invalid"
kubectl get configmap dlppolicy-smoke-good >/dev/null || fail "customRules ConfigMap not projected"
echo "✓ validation + projection OK"

step "5. Graph: Model + echo Skill + Agent with --dlp-policy"
bin/bairav model deploy Qwen/Qwen2.5-0.5B-Instruct-GGUF --target k8s
bin/bairav skill deploy nginx:alpine --name echo --port 80 --health-path / --target k8s
bin/bairav agent deploy nginx:alpine --name smoke-agent --port 80 --health-path / --target k8s \
  --model qwen2-5-0-5b --skill echo --dlp-policy smoke-good
kubectl get pod -l orch.bairavs.com/agent=smoke-agent
CONTAINERS=$(kubectl get pod -l orch.bairavs.com/agent=smoke-agent -o jsonpath='{.items[0].spec.containers[*].name}')
echo "containers: $CONTAINERS"
[[ "$CONTAINERS" == *dlp-qwen2-5-0-5b* ]] || fail "DLP sidecar not injected"
ENDPOINT=$(kubectl exec deploy/smoke-agent -c agent -- printenv BAIRAV_MODEL_QWEN2_5_0_5B_ENDPOINT)
[ "$ENDPOINT" = "http://127.0.0.1:9100/v1" ] || fail "endpoint not rewritten (got $ENDPOINT)"
echo "✓ sidecar + endpoint rewrite OK"

step "6. Traffic through the sidecar (request + response + streaming)"
kubectl port-forward deploy/smoke-agent 19100:9100 >/dev/null 2>&1 &
PF=$!; trap 'kill $PF 2>/dev/null || true' EXIT; sleep 3
curl -s http://127.0.0.1:19100/v1/chat/completions -H 'Content-Type: application/json' \
  -d '{"messages":[{"role":"user","content":"Repeat exactly: jane@corp.com TKT-123456"}],"max_tokens":40}' | tee /tmp/smoke-nonstream.json
echo
curl -sN http://127.0.0.1:19100/v1/chat/completions -H 'Content-Type: application/json' \
  -d '{"messages":[{"role":"user","content":"Repeat exactly: bob@corp.com"}],"max_tokens":40,"stream":true}' | tee /tmp/smoke-stream.txt | tail -3
echo
kubectl logs deploy/smoke-agent -c dlp-qwen2-5-0-5b --tail=5
grep -q "jane@corp.com" /tmp/smoke-nonstream.json && fail "raw email in non-stream response" || true
grep -q "bob@corp.com" /tmp/smoke-stream.txt && fail "raw email in streamed response" || true
echo "✓ no raw PII reached the caller (model may paraphrase — check audit lines above for hits)"

step "7. visualize"
bin/bairav visualize

step "8. policy edit rolls the Pod (hash annotation)"
OLD=$(kubectl get deploy smoke-agent -o jsonpath='{.spec.template.metadata.annotations.orch\.bairavs\.com/dlp-policy-hash}')
kubectl patch dlp smoke-good --type merge -p '{"spec":{"defaultAction":"block"}}'
sleep 5
NEW=$(kubectl get deploy smoke-agent -o jsonpath='{.spec.template.metadata.annotations.orch\.bairavs\.com/dlp-policy-hash}')
[ "$OLD" != "$NEW" ] || fail "policy hash did not change after edit"
echo "✓ hash $OLD → $NEW"

step "cleanup"
bin/bairav agent undeploy smoke-agent -f --target k8s
kubectl delete dlp smoke-good smoke-bad
printf '\n\033[1;32mALL SMOKE STEPS PASSED\033[0m\n'
