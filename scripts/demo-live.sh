#!/usr/bin/env bash
# Presenter script for demo video cut B (docs/demo-video/README.md).
# Prints each command, waits for Enter, runs it. Run from the orch-engine
# root against a `make dev` cluster, with `bin/bairav dashboard` open on
# the other half of the screen.
#
#   bash scripts/demo-live.sh          # the walkthrough
#   bash scripts/demo-live.sh --reset  # tear everything down for a retake
set -uo pipefail
cd "$(dirname "$0")/.."
B=bin/bairav

if [ "${1:-}" = "--reset" ]; then
  $B agent undeploy support-bot --target k8s --namespace team-a -f 2>/dev/null
  kubectl -n team-a delete dlppolicy prod-guardrails --ignore-not-found
  $B revoke team-a -n shared-models 2>/dev/null
  $B skill undeploy tickets --target k8s --namespace team-a -f 2>/dev/null
  $B skill undeploy kb-search --target k8s --namespace team-a -f 2>/dev/null
  $B model undeploy qwen --target k8s --namespace shared-models -f 2>/dev/null
  echo "reset done"; exit 0
fi

say()  { printf '\n\033[1;33m# %s\033[0m\n' "$*"; }
run()  { printf '\033[1m$ %s\033[0m' "$*"; read -r _; eval "$@"; }
wait_agent() { # wait_agent <phase>
  for _ in $(seq 1 90); do
    [ "$(kubectl -n team-a get agent support-bot -o jsonpath='{.status.phase}' 2>/dev/null)" = "$1" ] && return 0; sleep 2
  done
}

clear
say "1. Platform team deploys one shared model"
run $B model deploy Qwen/Qwen2.5-0.5B-Instruct-GGUF --target k8s --name qwen --namespace shared-models

say "2. team-a ships two MCP tools as Skills"
run $B skill deploy bairav/mcp-echo:v1 --target k8s --namespace team-a --name tickets
run $B skill deploy bairav/mcp-echo:v1 --target k8s --namespace team-a --name kb-search

say "3. team-a's agent references the shared model: watch it wait"
run $B agent deploy nginx:alpine --target k8s --namespace team-a --name support-bot --port 80 --health-path / \
  --model shared-models/qwen --skill tickets --skill kb-search --wait=false
wait_agent WaitingForDeps
run "kubectl -n team-a get agent support-bot -o jsonpath='{.status.conditions[?(@.type==\"Progressing\")].message}'; echo"

say "4. The model owner approves"
run $B grant team-a --model qwen -n shared-models
wait_agent Ready

say "5. Governance: a DLP policy, attached as a sidecar"
run "cat <<'EOF' | kubectl apply -f -
apiVersion: orch.bairavs.com/v1alpha1
kind: DLPPolicy
metadata: { name: prod-guardrails, namespace: team-a }
spec:
  packs: [global-safe, india]
  defaultAction: redact
  direction: both
EOF"
run $B agent deploy nginx:alpine --target k8s --namespace team-a --name support-bot --port 80 --health-path / \
  --model shared-models/qwen --skill tickets --skill kb-search --dlp-policy prod-guardrails
wait_agent Ready

say "6. A prompt with an Aadhaar number goes through the sidecar"
kubectl -n team-a port-forward deploy/support-bot 19100:9100 >/dev/null 2>&1 &
PF=$!; trap 'kill $PF 2>/dev/null' EXIT; sleep 3
# 2345 6789 0124 passes Verhoeff and is written in the 4-4-4 Aadhaar layout next to the word "Aadhaar", so global-safe redacts it. (Checksum alone would also pass ~10% of random 12-digit numbers; the context rule is what stops that.)
run "curl -s http://127.0.0.1:19100/v1/chat/completions -H 'Content-Type: application/json' \
  -d '{\"messages\":[{\"role\":\"user\",\"content\":\"Customer Aadhaar 2345 6789 0124 asks about refunds\"}],\"max_tokens\":40}' | head -c 300; echo"
say "(dashboard → DLP activity shows: redact AADHAAR)"

say "7. And it's all just Kubernetes"
run "kubectl get models,skills,agents,dlppolicies,accessgrants -A"

say "Done. bairavs.com/orch-engine"
