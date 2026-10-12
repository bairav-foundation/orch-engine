#!/usr/bin/env bash
# Smoke test for Day 28: cloud models behind the DLP gateway.
#
# Needs no API keys: the "cloud" provider is openai-compatible, pointed at
# a local Qwen stack, so the full gateway path (pinning, DLP, path guard,
# fail-closed, local ⇄ cloud upgrade + rollback) runs offline.
# Real providers are tested too when you opt in:
#   OPENAI_API_KEY=... OPENAI_MODEL=<model id>   bash scripts/smoke-day28.sh
#   ANTHROPIC_API_KEY=... ANTHROPIC_MODEL=<id>   bash scripts/smoke-day28.sh
# k8s steps run when the Model CRD is installed on the current context.
set -euo pipefail
cd "$(dirname "$0")/.."

step() { printf '\n\033[1;34m== %s\033[0m\n' "$*"; }
fail() { printf '\n\033[1;31mFAIL: %s\033[0m\n' "$*"; exit 1; }
B=bin/bairav
UP=d28-qwen
GW=d28-gw
QWEN=Qwen/Qwen2.5-0.5B-Instruct-GGUF

step "0. make fmt + rebuild + gateway image"
make fmt
make rebuild
make image-dlp

cleanup() {
  for n in $GW $UP d28-openai d28-anthropic; do $B model undeploy $n --force >/dev/null 2>&1 || true; done
}
trap cleanup EXIT
cleanup

wait_healthy() { # name
  for _ in $(seq 1 120); do
    docker ps --format '{{.Names}} {{.Status}}' | grep -E "^bairav-$1 " | grep -q "(healthy)" && return 0; sleep 3
  done
  fail "bairav-$1 never became healthy"
}

step "1. docker: local Qwen as the 'provider' + an openai-compatible gateway in front"
$B model deploy $QWEN --name $UP --port 18280
wait_healthy $UP
$B model deploy qwen-local --provider openai-compatible --base-url "http://model-$UP:8080/v1" --name $GW --port 18281
wait_healthy $GW
G=http://127.0.0.1:18281

step "2. gateway behaviour"
curl -sf $G/v1/models | grep -q '"qwen-local"' || fail "/v1/models should list only the pinned model"
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST $G/v1/files -d '{}')
[ "$CODE" = "404" ] || fail "/v1/files should be refused (got $CODE)"
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST $G/v1/chat/completions -d 'not json')
[ "$CODE" = "400" ] || fail "unparseable body should be refused (got $CODE)"
curl -sf $G/v1/chat/completions -H 'Content-Type: application/json' -H 'Authorization: Bearer sk-callers-own' \
  -d '{"model":"anything","max_tokens":8,"messages":[{"role":"user","content":[{"type":"text","text":"Mail alice@example.com, reply OK"}]}]}' \
  | grep -q '"choices"' || fail "chat through the gateway failed"
sleep 1
docker logs bairav-$GW 2>&1 | grep '"path":"/v1/chat/completions"' | tail -1 | grep -q 'EMAIL' \
  || fail "audit has no EMAIL hit for the content-part prompt"
echo "✓ pinned model, path guard, fail-closed, content-part DLP + audit"

step "3. model status shows the provider"
$B model status | grep -E "^$GW +docker" | grep -q "openai-compatible:qwen-local" || fail "status should show openai-compatible:qwen-local"
echo "✓ status"

step "4. cloud → local upgrade keeps DLP, rollback restores the gateway"
$B model upgrade $GW --to $QWEN --provider local --rollout-timeout 10m
docker ps --format '{{.Names}}' | grep -q "^bairav-$GW-dlp$" || fail "cloud → local dropped DLP (no dlp container)"
$B model status $GW | grep -q "Previous:.*qwen-local" || fail "previous should be the gateway model"
$B model rollback $GW --rollout-timeout 5m
$B model status | grep -E "^$GW +docker" | grep -q "openai-compatible:qwen-local" || fail "rollback didn't restore the gateway"
docker ps --format '{{.Names}}' | grep -q "^bairav-$GW-dlp$" && fail "dlp sidecar left behind after rollback"
echo "✓ local ⇄ cloud swap both ways"

if [ -n "${OPENAI_API_KEY:-}" ] && [ -n "${OPENAI_MODEL:-}" ]; then
  step "5a. real OpenAI ($OPENAI_MODEL)"
  $B model deploy "$OPENAI_MODEL" --provider openai --name d28-openai --port 18282
  wait_healthy d28-openai
  grep -rq "$OPENAI_API_KEY" ~/.bairav/compose/d28-openai/docker-compose.yaml && fail "key leaked into compose file"
  curl -sf http://127.0.0.1:18282/v1/chat/completions -H 'Content-Type: application/json' \
    -d '{"messages":[{"role":"user","content":"Reply with OK"}],"max_tokens":5}' | grep -q '"choices"' \
    || fail "OpenAI call through the gateway failed"
  echo "✓ OpenAI"
fi
if [ -n "${ANTHROPIC_API_KEY:-}" ] && [ -n "${ANTHROPIC_MODEL:-}" ]; then
  step "5b. real Anthropic ($ANTHROPIC_MODEL), native /v1/messages"
  $B model deploy "$ANTHROPIC_MODEL" --provider anthropic --name d28-anthropic --port 18283
  wait_healthy d28-anthropic
  curl -sf http://127.0.0.1:18283/v1/messages -H 'Content-Type: application/json' \
    -d '{"model":"x","max_tokens":20,"messages":[{"role":"user","content":"Repeat: my email is bob@example.org"}]}' \
    | tee /tmp/d28-ant.json | grep -q '"content"' || fail "Anthropic /v1/messages through the gateway failed"
  grep -q "bob@example.org" /tmp/d28-ant.json && fail "email reached / came back from Anthropic unredacted"
  echo "✓ Anthropic"
fi

if kubectl get crd models.orch.bairavs.com >/dev/null 2>&1; then
  NS=bairav-d28
  kclean() { kubectl delete ns $NS --wait=false >/dev/null 2>&1 || true; }
  trap 'cleanup; kclean' EXIT

  step "6. k8s: operator + CRDs + images"
  make install-operator
  make image-serving
  kubectl -n bairav-system rollout restart deploy/bairav-operator
  kubectl -n bairav-system rollout status deploy/bairav-operator --timeout 3m
  kubectl get crd models.orch.bairavs.com -o yaml | grep -q "openai-compatible" || fail "CRD has no provider field; CRDs not updated"
  kubectl delete ns $NS --wait=true >/dev/null 2>&1 || true
  kubectl create ns $NS

  step "7. k8s: invalid cloud spec is refused with a fix-it message"
  kubectl apply -f - <<EOF
apiVersion: orch.bairavs.com/v1alpha1
kind: Model
metadata: { name: nokey, namespace: $NS }
spec: { provider: openai, repo: gpt-x }
EOF
  for _ in $(seq 1 20); do
    [ "$(kubectl get model nokey -n $NS -o jsonpath='{.status.phase}')" = "Failed" ] && break; sleep 2
  done
  kubectl get model nokey -n $NS -o jsonpath='{.status.conditions[?(@.reason=="InvalidSpec")].message}' | grep -q apiKeySecretRef \
    || fail "missing key should fail with an apiKeySecretRef hint"
  echo "✓ InvalidSpec"

  step "8. k8s: gateway in front of an in-cluster Qwen"
  $B model deploy $QWEN --name $UP --target k8s --namespace $NS --timeout 15m
  $B model deploy qwen-local --provider openai-compatible --base-url "http://$UP.$NS.svc.cluster.local:8080/v1" \
    --name $GW --target k8s --namespace $NS --timeout 5m
  [ "$(kubectl get deploy $GW -n $NS -o jsonpath='{.spec.template.spec.containers[0].name}')" = "gateway" ] || fail "not a gateway pod"
  kubectl get --raw "/api/v1/namespaces/$NS/services/$GW:http/proxy/v1/models" | grep -q qwen-local || fail "gateway /v1/models via service proxy"
  echo "✓ gateway Ready"

  step "9. k8s: cloud → local → rollback"
  $B model upgrade $GW --to $QWEN --provider local --target k8s --namespace $NS --rollout-timeout 15m
  [ "$(kubectl get model $GW -n $NS -o jsonpath='{.spec.provider}')" = "local" ] || fail "provider not local"
  $B model rollback $GW --target k8s --namespace $NS --rollout-timeout 5m
  [ "$(kubectl get model $GW -n $NS -o jsonpath='{.spec.provider}')" = "openai-compatible" ] || fail "rollback lost the provider"
  [ "$(kubectl get model $GW -n $NS -o jsonpath='{.spec.cloud.baseURL}')" != "" ] || fail "rollback lost cloud.baseURL"
  echo "✓ k8s swap both ways"
else
  echo; echo "(no Model CRD on the current context; skipped k8s steps 6-9)"
fi

printf '\n\033[1;32mDAY 28 SMOKE PASSED\033[0m\n'
