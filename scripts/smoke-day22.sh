#!/usr/bin/env bash
# Smoke test for: cross-namespace refs + AccessGrant on k8s.
# Needs a running `make dev` cluster. Run from the orch-engine root:
#   bash scripts/smoke-day22.sh
set -euo pipefail
cd "$(dirname "$0")/.."

step() { printf '\n\033[1;34m== %s\033[0m\n' "$*"; }
fail() { printf '\n\033[1;31mFAIL: %s\033[0m\n' "$*"; exit 1; }
B=bin/bairav
phase() { kubectl -n team-b get agent bot -o jsonpath='{.status.phase}'; }
wait_phase() { # wait_phase <want> <seconds>
  for _ in $(seq 1 "$2"); do [ "$(phase)" = "$1" ] && return 0; sleep 1; done
  fail "agent phase is '$(phase)', wanted '$1'"
}

step "0. make fmt + ci, rebuild + operator image"
make fmt
make rebuild-all
kubectl apply -f operator/config/crd/bases/
kubectl apply -f operator/config/rbac/
kubectl -n bairav-system rollout restart deploy/bairav-operator
kubectl -n bairav-system rollout status deploy/bairav-operator --timeout=120s

step "1. team-a owns a Skill (cheap stand-in for a Model: same code path)"
$B skill deploy nginx:alpine --target k8s --namespace team-a --name echo --port 80 --health-path /

step "2. team-b agent references team-a/echo with NO grant → WaitingForDeps + fix hint"
$B agent deploy nginx:alpine --target k8s --namespace team-b --name bot --port 80 --health-path / \
  --skill team-a/echo --wait=false
wait_phase WaitingForDeps 30
MSG=$(kubectl -n team-b get agent bot -o jsonpath='{.status.conditions[?(@.type=="Progressing")].message}')
echo "message: $MSG"
[[ "$MSG" == *"bairav grant team-b --skill echo -n team-a"* ]] || fail "hint missing from condition"

step "3. grant → agent becomes Ready without redeploy"
$B grant team-b --skill echo -n team-a
wait_phase Ready 120
kubectl -n team-b exec deploy/bot -- printenv BAIRAV_SKILL_ECHO_ENDPOINT | grep -q "echo.team-a.svc" || fail "endpoint not wired to team-a"
kubectl -n team-b get agent bot -o jsonpath='{.status.boundSkills}' | grep -q "team-a/echo" || fail "boundSkills should show team-a/echo"
echo "✓ granted"

step "4. grants list + visualize"
$B grants -A
$B visualize | grep -q "S_team_a_echo" || fail "visualize missing cross-namespace edge target"

step "5. revoke → back to WaitingForDeps"
$B revoke team-b -n team-a
wait_phase WaitingForDeps 30
echo "✓ revoked"

step "6. same-namespace refs unaffected; cross-ns dlpRef rejected"
cat <<'EOF' | kubectl apply -f -
apiVersion: orch.bairavs.com/v1alpha1
kind: Agent
metadata: { namespace: team-b, name: bad-dlp }
spec:
  image: nginx:alpine
  port: 80
  healthPath: /
  dlpRef: { namespace: team-a, name: anything }
EOF
sleep 5
kubectl -n team-b get agent bad-dlp -o jsonpath='{.status.conditions[?(@.type=="Progressing")].message}' | grep -q "own namespace" \
  || fail "cross-namespace dlpRef not rejected"
echo "✓ cross-ns dlpRef rejected"

step "cleanup"
kubectl -n team-b delete agent bot bad-dlp --ignore-not-found
$B skill undeploy echo --target k8s --namespace team-a -f
printf '\n\033[1;32mDAY 22 SMOKE PASSED\033[0m\n'
