#!/usr/bin/env python3
# Renders docs/images/terminal.svg: an animated terminal (SMIL, no JS) that
# plays the main Orch Engine flow. It works as an <img> in the GitHub README.
#
#   python3 scripts/gen-terminal-svg.py
#
# Edit SCRIPT below to change what it shows. Output text is illustrative of
# real CLI output, trimmed to fit.
import os
from xml.sax.saxutils import escape

SCRIPT = [
    ("cmd", "curl -fsSL https://bairavs.com/install.sh | sh"),
    ("out", "Downloading bairav v0.2.0 (darwin/arm64)... checksum OK"),
    ("out", "Installed bairav v0.2.0 to /usr/local/bin/bairav"),
    ("gap", ""),
    ("cmd", "kubectl apply -f https://github.com/bairav-foundation/orch-engine/releases/latest/download/install.yaml"),
    ("out", "customresourcedefinition.apiextensions.k8s.io/models.orch.bairavs.com created   (+4 more)"),
    ("out", "deployment.apps/bairav-operator created"),
    ("gap", ""),
    ("cmd", "bairav model deploy Qwen/Qwen2.5-0.5B-Instruct-GGUF --target k8s --name qwen --dlp-ref guard"),
    ("out", "  Phase: Pulling    4s"),
    ("ok",  "✓ Ready: http://qwen.default.svc.cluster.local:8080/v1   (DLP enforced inside the model pod)"),
    ("gap", ""),
    ("cmd", "curl -s localhost:8080/v1/chat/completions -d '{\"messages\":[{\"role\":\"user\",\"content\":"),
    ("cmd2", "      \"Repeat: my aadhaar is 2345 6789 0124, key AKIAIOSFODNN7EXAMPLE\"}]}' | jq .choices[0].message.content"),
    ("ok",  "\"My aadhaar is [REDACTED:AADHAAR], key [REDACTED:AWS_ACCESS_KEY]\""),
    ("gap", ""),
    ("cmd", "bairav model upgrade qwen --to Qwen/Qwen3-1.7B-GGUF --target k8s --eval support.yaml"),
    ("out", "  Eval gate: support (12 cases) ...   ✗ json-output   ✗ refund-policy"),
    ("bad", "✗ Score 10/12, needs 100%: rolled back. qwen still serves Qwen2.5-0.5B, agents untouched"),
    ("gap", ""),
    ("cmd", "bairav dlp serve --litellm-guardrail --pack global-safe,india"),
    ("out", "  mode:      LiteLLM guardrail (POST /beta/litellm_basic_guardrail_api)"),
    ("out", "  packs:     global-safe, india   audit: masked"),
]

W, LH, PAD, TOP = 900, 21, 18, 46
CHAR = 7.5                      # approx width of one 12.5px monospace char
TYPE = 0.022                    # seconds per typed char
HOLD = 5.0                      # seconds the finished screen stays up

COLORS = {"cmd": "#e8eef5", "cmd2": "#e8eef5", "out": "#9fb0c3", "ok": "#6fd3c5", "bad": "#f4a259", "gap": "#000"}

# Timeline: when each line starts and finishes appearing.
t, lines = 0.6, []
for kind, text in SCRIPT:
    if kind == "gap":
        t += 0.35
        lines.append((kind, text, t, t))
        continue
    if kind in ("cmd", "cmd2"):
        d = min(1.8, max(0.4, len(text) * TYPE))
        lines.append((kind, text, t, t + d))
        t += d + (0.25 if kind == "cmd" else 0.6)
    else:
        lines.append((kind, text, t, t))
        t += 0.3
T = t + HOLD
H = TOP + LH * len(SCRIPT) + PAD


def kt(x):
    return f"{max(0.0, min(1.0, x / T)):.4f}"


body = []
for i, (kind, text, s, e) in enumerate(lines):
    if kind == "gap":
        continue
    y = TOP + LH * i
    prompt = kind == "cmd"
    full = (len(text) + (2 if prompt else 0)) * CHAR + 4
    # Visible from s until just before the loop restarts; typed lines reveal
    # left to right through a growing clip rectangle.
    vis = (f'<animate attributeName="opacity" dur="{T:.2f}s" repeatCount="indefinite" calcMode="discrete" '
           f'keyTimes="0;{kt(s)};{kt(T - 0.4)}" values="0;1;0"/>')
    clip_id = f"c{i}"
    if kind in ("cmd", "cmd2"):
        body.append(f'<clipPath id="{clip_id}"><rect x="{PAD}" y="{y - 15}" height="{LH}" width="0">'
                    f'<animate attributeName="width" dur="{T:.2f}s" repeatCount="indefinite" '
                    f'keyTimes="0;{kt(s)};{kt(e)};{kt(T - 0.4)};1" values="0;0;{full:.0f};{full:.0f};0"/></rect></clipPath>')
        clip = f' clip-path="url(#{clip_id})"'
    else:
        clip = ""
    spans = ""
    if prompt:
        spans += '<tspan fill="#2a9d8f">$ </tspan>'
    spans += f'<tspan fill="{COLORS[kind]}">{escape(text)}</tspan>'
    body.append(f'<text x="{PAD}" y="{y}"{clip} opacity="0">{vis}{spans}</text>')

svg = f'''<svg xmlns="http://www.w3.org/2000/svg" width="{W}" height="{H}" viewBox="0 0 {W} {H}" role="img" aria-label="Terminal: install the bairav CLI, install the operator, deploy a model with DLP inside its pod, see an Aadhaar number and an AWS key redacted, an upgrade rolled back by an eval gate, and DLP started as a LiteLLM guardrail">
<title>BAIRAVS Orch Engine in one minute</title>
<style>text{{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:12.5px;white-space:pre}}</style>
<rect width="{W}" height="{H}" rx="12" fill="#142030"/>
<rect width="{W}" height="32" rx="12" fill="#1c2b3f"/><rect y="20" width="{W}" height="12" fill="#1c2b3f"/>
<circle cx="20" cy="16" r="5.5" fill="#ec6a5e"/><circle cx="38" cy="16" r="5.5" fill="#f4bf4f"/><circle cx="56" cy="16" r="5.5" fill="#61c554"/>
<text x="{W / 2}" y="20" text-anchor="middle" fill="#8d98a8" style="font-size:12px">bairav · orch engine</text>
{chr(10).join(body)}
</svg>
'''

out = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "docs", "images", "terminal.svg")
os.makedirs(os.path.dirname(out), exist_ok=True)
open(out, "w", encoding="utf-8").write(svg)
print(f"wrote {out} ({len(svg)} bytes, loop {T:.1f}s)")
