#!/usr/bin/env bash
# Smoke test for: bairav dashboard. Works with or without a cluster
# (live checks adapt). Run from the orch-engine root:
#   bash scripts/smoke-day23.sh
set -euo pipefail
cd "$(dirname "$0")/.."

step() { printf '\n\033[1;34m== %s\033[0m\n' "$*"; }
fail() { printf '\n\033[1;31mFAIL: %s\033[0m\n' "$*"; exit 1; }
B=bin/bairav

step "0. make fmt + rebuild (ci + CLI)"
make fmt
make rebuild

step "1. export the static demo"
OUT=$(mktemp -d)
$B dashboard --export-demo "$OUT"
[ -s "$OUT/index.html" ] || fail "export missing"
grep -q "BAIRAV_FRAMES" "$OUT/index.html" && fail "placeholder not replaced"
grep -q "fresh cluster" "$OUT/index.html" || fail "demo captions not inlined"
echo "✓ $OUT/index.html ($(wc -c < "$OUT/index.html") bytes). Open it in a browser to watch the demo."

step "2. live dashboard serves the page + snapshot"
$B dashboard --listen 127.0.0.1:17777 >/tmp/bairav-dash.log 2>&1 &
PID=$!; trap 'kill $PID 2>/dev/null || true' EXIT
sleep 2
curl -sf http://127.0.0.1:17777/ | grep -q "BAIRAVS Orch Engine" || fail "page not served"
curl -sf http://127.0.0.1:17777/api/snapshot > /tmp/bairav-snap.json || fail "snapshot endpoint"
python3 - <<'EOF'
import json
s=json.load(open("/tmp/bairav-snap.json"))
assert s["mode"]=="live", s["mode"]
for k in ("models","skills","agents","dlpPolicies","grants","audit"):
    assert isinstance(s[k], list), k
print(f"✓ snapshot: {len(s['models'])} models, {len(s['skills'])} skills, {len(s['agents'])} agents, "
      f"{len(s['grants'])} grants, {len(s['audit'])} DLP events; context={s.get('context') or '-'}")
for w in s.get("warnings") or []: print("  warning:", w)
EOF
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST http://127.0.0.1:17777/api/snapshot)
[ "$CODE" = "405" ] || fail "POST should be 405 (read-only), got $CODE"
curl -sI http://127.0.0.1:17777/ | grep -qi "content-security-policy" || fail "CSP header missing"
echo "✓ read-only + CSP"

step "3. demo mode exposes no live API"
kill $PID; sleep 1
$B dashboard --demo --listen 127.0.0.1:17778 >/dev/null 2>&1 &
PID=$!
sleep 2
CODE=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:17778/api/snapshot)
[ "$CODE" = "404" ] || fail "demo mode must not serve /api/snapshot (got $CODE)"
echo "✓ demo isolated"

printf '\n\033[1;32mDAY 23 SMOKE PASSED\033[0m  Now open: bin/bairav dashboard\n'
