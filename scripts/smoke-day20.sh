#!/usr/bin/env bash
# Smoke test for Day 20: Model + Skill + Agent on the docker target, no
# cluster. Needs Go + docker. Run from the orch-engine root:
#   bash scripts/smoke-day20.sh
set -euo pipefail
cd "$(dirname "$0")/.."

step() { printf '\n\033[1;34m== %s\033[0m\n' "$*"; }
fail() { printf '\n\033[1;31mFAIL: %s\033[0m\n' "$*"; exit 1; }
B=bin/bairav

step "0. make fmt + vet + tests"
make fmt || fail "make fmt"
(cd cli && go vet ./... && go test ./internal/compose/... ./cmd/...) || fail "cli vet/test"

step "1. build CLI + images"
make rebuild || fail "make rebuild"
make image-dlp || fail "image-dlp"
docker build -t bairav/mcp-echo:v1 examples/mcp-echo || fail "mcp-echo build"

step "2. Model (enforce mode) + Skill on docker"
$B model deploy Qwen/Qwen2.5-0.5B-Instruct-GGUF --target docker --name qwen --port 18080 --dlp
$B skill deploy bairav/mcp-echo:v1 --name echo --target docker
docker network inspect bairav >/dev/null || fail "shared network 'bairav' missing"

step "3. Agent referencing both, with agent-side DLP"
$B agent deploy nginx:alpine --target docker --name smoke --port 80 \
  --model qwen --skill echo --dlp --dlp-pack global-safe
A=bairav-agent-smoke
docker exec $A printenv BAIRAV_MODEL_QWEN_ENDPOINT | grep -q "http://dlp-qwen:8090/v1" || fail "model endpoint not rewritten to agent DLP proxy"
docker exec $A printenv BAIRAV_SKILL_ECHO_ENDPOINT | grep -q "http://skill-echo:3000" || fail "skill endpoint wrong"

step "4. Network wiring from inside the agent container"
docker exec $A wget -qO- http://skill-echo:3000/health | grep -q ok || fail "agent cannot reach skill-echo"
echo "✓ agent → skill"
echo "waiting for the model to load (first boot pulls the GGUF) ..."
for i in $(seq 1 60); do
  docker exec $A wget -qO- http://model-qwen:8090/v1/models >/dev/null 2>&1 && break
  sleep 5
done
docker exec $A wget -qO- --header 'Content-Type: application/json' \
  --post-data '{"messages":[{"role":"user","content":"Repeat exactly: jane@corp.com"}],"max_tokens":30}' \
  http://dlp-qwen:8090/v1/chat/completions | tee /tmp/smoke20.json; echo
grep -q "jane@corp.com" /tmp/smoke20.json && fail "raw email came back through the agent DLP proxy" || true
echo "✓ agent → dlp-qwen → model-qwen (model's own enforce proxy)"

step "5. DLP bypass check: serving must be unreachable in enforce mode"
if docker exec $A wget -qO- -T 3 http://bairav-qwen:8080/health >/dev/null 2>&1; then
  fail "agent reached the raw model container — enforce mode bypass"
fi
echo "✓ raw model not reachable from the shared network"

step "6. ps"
$B ps --no-k8s

step "7. undeploy model without cascade lists the dependent, then cascade"
printf 'no\n' | $B model undeploy qwen --target docker | tee /tmp/smoke20-undeploy.txt
grep -q "smoke" /tmp/smoke20-undeploy.txt || fail "dependent agent not listed"
$B model undeploy qwen --target docker --cascade-agents -f
docker ps -a --format '{{.Names}}' | grep -q "^$A\$" && fail "agent not cascade-deleted" || true
echo "✓ cascade"

step "cleanup"
$B skill undeploy echo --target docker -f
printf '\n\033[1;32mDAY 20 SMOKE PASSED\033[0m\n'
