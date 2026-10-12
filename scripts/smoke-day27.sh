#!/usr/bin/env bash
# Smoke test for: bairav model upgrade / rollback.
# Docker path always; k8s path too when a cluster is reachable and the
# operator + CRDs are installed (make deploy). Run from the orch-engine root:
#   bash scripts/smoke-day27.sh
# Uses two tiny models (~400 MB + ~1 GB). Set FROM/TO to override.
set -euo pipefail
cd "$(dirname "$0")/.."

step() { printf '\n\033[1;34m== %s\033[0m\n' "$*"; }
fail() { printf '\n\033[1;31mFAIL: %s\033[0m\n' "$*"; exit 1; }
B=bin/bairav
FROM=${FROM:-Qwen/Qwen2.5-0.5B-Instruct-GGUF}
TO=${TO:-Qwen/Qwen2.5-1.5B-Instruct-GGUF}
NAME=upg-smoke
PORT=18127

step "0. make fmt + rebuild (ci + CLI)"
make fmt
make rebuild

cleanup() { $B model undeploy $NAME --force >/dev/null 2>&1 || true; }
trap cleanup EXIT

step "0b. model discover (needs internet)"
if $B model discover --limit 5; then echo "✓ discover"; else echo "(discover failed: offline? not fatal)"; fi

step "1. docker: deploy $FROM as $NAME"
cleanup
$B model deploy "$FROM" --name $NAME --port $PORT --dlp
for _ in $(seq 1 120); do
  docker ps --format '{{.Names}} {{.Status}}' | grep -q "bairav-$NAME.*(healthy)" && break; sleep 5
done
curl -sf http://127.0.0.1:$PORT/v1/models >/dev/null || fail "initial model not serving"
echo "✓ serving $FROM"

step "2. docker: upgrade to $TO (port + DLP must survive)"
$B model upgrade $NAME --to "$TO"
docker ps --format '{{.Names}} {{.Labels}}' | grep "bairav-$NAME" | grep -q "$TO" || fail "container not on $TO"
docker ps --format '{{.Names}}' | grep -q "bairav-$NAME.*dlp" || fail "DLP sidecar lost on upgrade"
curl -sf http://127.0.0.1:$PORT/v1/models >/dev/null || fail "port $PORT lost on upgrade"
echo "✓ upgraded; DLP + port kept"
$B model status | grep -E "^$NAME +docker" | grep -q "$TO" || fail "model status doesn't show $NAME on $TO"
$B model status $NAME | grep -q "Previous:.*$FROM" || fail "model status $NAME doesn't show previous model"
echo "✓ model status shows serving + previous + rollback hint"

step "3. docker: failing upgrade auto-rolls back"
if $B model upgrade $NAME --to bairav-foundation/does-not-exist-GGUF --rollout-timeout 3m; then
  fail "upgrade to a missing repo should fail"
fi
docker ps --format '{{.Names}} {{.Labels}}' | grep "bairav-$NAME" | grep -q "$TO" || fail "not rolled back to $TO"
echo "✓ bad upgrade rolled back to $TO"

step "4. docker: rollback"
$B model rollback $NAME
docker ps --format '{{.Names}} {{.Labels}}' | grep "bairav-$NAME" | grep -q "$FROM" || fail "rollback didn't restore $FROM"
echo "✓ rolled back to $FROM"
cleanup

if kubectl get crd models.orch.bairavs.com >/dev/null 2>&1; then
  NS=bairav-upg-smoke
  kcleanup() { kubectl delete ns $NS --wait=false >/dev/null 2>&1 || true; }
  trap 'cleanup; kcleanup' EXIT

  step "5a. k8s: current operator + CRDs, serving image on every node"
  make install-operator
  # Local images must be imported into every k3d node (nodes added after
  # an earlier import don't have them: ImagePullBackOff on bairav/serving).
  make image-serving
  kubectl -n bairav-system rollout restart deploy/bairav-operator
  kubectl -n bairav-system rollout status deploy/bairav-operator --timeout 3m
  kubectl get crd models.orch.bairavs.com -o yaml | grep -q servedRepo || fail "models CRD has no servedRepo; CRDs not updated"

  step "5. k8s: deploy $FROM, surge upgrade to $TO"
  kubectl delete ns $NS --wait=true >/dev/null 2>&1 || true   # leftovers from a hung run
  kubectl create ns $NS >/dev/null 2>&1 || true
  $B model deploy "$FROM" --name $NAME --target k8s --namespace $NS --timeout 15m
  $B model upgrade $NAME --to "$TO" --target k8s --namespace $NS --rollout-timeout 15m
  [ "$(kubectl get model $NAME -n $NS -o jsonpath='{.status.servedRepo}')" = "$TO" ] || fail "status.servedRepo not $TO"
  [ "$(kubectl get deploy $NAME -n $NS -o jsonpath='{.spec.strategy.type}')" = "RollingUpdate" ] || fail "default strategy should be RollingUpdate"
  echo "✓ surge upgrade; servedRepo=$TO"

  step "6. k8s: rollback with --strategy recreate saved for next time"
  $B model rollback $NAME --target k8s --namespace $NS --rollout-timeout 15m
  [ "$(kubectl get model $NAME -n $NS -o jsonpath='{.status.servedRepo}')" = "$FROM" ] || fail "rollback didn't restore $FROM"
  $B model upgrade $NAME --to "$TO" --target k8s --namespace $NS --strategy recreate --rollout-timeout 15m
  [ "$(kubectl get deploy $NAME -n $NS -o jsonpath='{.spec.strategy.type}')" = "Recreate" ] || fail "recreate not applied"
  echo "✓ rollback + recreate strategy"
else
  echo; echo "(no Model CRD on the current context; skipped k8s steps 5-6)"
fi

printf '\n\033[1;32mDAY 27 SMOKE PASSED\033[0m\n'
