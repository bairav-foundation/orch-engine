# Demo video kit

Two cuts, both recorded against `bairav dashboard`. Record cut A first: it needs no cluster and is the one the website leads with.

| Cut | Length | Needs | Use |
|-----|--------|-------|-----|
| **A — Teaser** | ~50 s | nothing (demo mode) | bairavs.com/orch-engine hero, socials, README GIF |
| **B — The real thing** | ~2.5 min | k3d cluster from `make dev` | product page "see it work", talks, investors |

## Setup (both cuts)

- Screen: 1920×1080 (or record a 1920×1080 window). Hide the dock and desktop icons, and turn on Do Not Disturb.
- Browser: a fresh window with no bookmarks bar and zoom at **125%** so the graph reads on a phone. Use the light theme (the brand look).
- Recorder: QuickTime (Mac: ⌘⇧5 → Record Selected Portion) or OBS. 30 fps is plenty.
- Audio: record the voice-over separately and lay it on afterwards. It's easier than talking while clicking. The scripts below are timed to the demo frames.
- For cut B, use a terminal font size of 18+ and a plain prompt (`PS1='$ '`).

---

## Cut A — Teaser (demo mode, ~50 s)

```bash
bin/bairav dashboard --demo        # open http://127.0.0.1:7777, then start recording
```

The demo auto-plays nine frames with captions. Let it play once without touching anything. To redo a step, use ◀ ▶ and pause ❚❚.

| Time | Frame (caption on screen) | Voice-over |
|------|---------------------------|------------|
| 0:00 | 1 · fresh cluster | "This is Orch Engine, from BAIRAVS. Your own AI platform, on your own Kubernetes." |
| 0:04 | 2–3 · model pulling → Ready | "One command deploys an open model, Qwen here, with an OpenAI-compatible API." |
| 0:11 | 4 · two MCP Skills | "Teams add tools as MCP Skills: tickets, knowledge search, whatever they build." |
| 0:15 | 5 · agent waiting | "An agent asks for the shared model. No permission yet, so it waits, and it tells you exactly why." |
| 0:21 | 6 · grant → Ready | "The owner grants access in one line. The agent is live in seconds." |
| 0:26 | 7 · DLP redacts | "Attach a DLP policy, and every prompt and every answer is checked. Aadhaar numbers and emails never reach the model or come back out." |
| 0:33 | 8 · AWS key blocked | "A leaked AWS key is blocked outright." |
| 0:39 | 9 · closing | "Models, Skills, Agents and DLP: one governed graph. Self-hosted, open, for everyone. bairavs.com/orch-engine" |

End with 2 s on frame 9, then fade. For a README GIF, crop to the graph card and export at 960 px wide.

---

## Cut B — The real thing (~2.5 min)

Split screen: terminal on the left (40%), `bin/bairav dashboard` on the right (60%). `scripts/demo-live.sh` drives the terminal. It prints each command, waits for Enter, then runs it, so you control the pace and nothing is typed live.

```bash
bash scripts/demo-live.sh          # prompts before every step; Ctrl-C any time
bash scripts/demo-live.sh --reset  # tear everything down for a retake
```

Prerequisite: `make rebuild-all`, so the operator, DLP sidecar and `bairav/mcp-echo:v1` images are in the cluster. Before recording, run it once all the way through so the model image and GGUF are cached. Cold pulls are slow and boring on camera. Then `--reset` and record.

| Step | What happens on screen | Voice-over |
|------|------------------------|------------|
| 1 | `model deploy … -n shared-models`; node appears, goes Ready | "Platform team: one shared model for the company." |
| 2 | two Skills in team-a | "team-a ships two MCP tools." |
| 3 | `agent deploy … --model shared-models/qwen`; dashed red edge, waiting | "Their agent references the shared model. Look: it's waiting, and the reason is a missing grant. Here's the exact command." |
| 4 | `bairav grant team-a --model qwen -n shared-models`; edge goes solid | "Owner approves. No redeploy; it just binds." |
| 5 | apply DLPPolicy, redeploy agent with `--dlp-policy`; 🛡 appears | "Now governance. A DLP policy, attached as a sidecar. The agent's code doesn't change." |
| 6 | a prompt with an Aadhaar number through the sidecar; DLP activity lights up with `redact AADHAAR` | "Sensitive data in, redacted before the model sees it. Every decision is audited." |
| 7 | `bairav visualize` (optional) | "And it's all just Kubernetes: kubectl, GitOps, your RBAC." |
| 8 | closing shot of the dashboard | "Orch Engine. Install it in a minute: bairavs.com/orch-engine" |

## After recording

- Export at 1080p H.264, under 50 MB for the site. Make a 720p copy for socials.
- Put the files in the website repo under `public/orch-engine/video/`. The `/orch-engine` page has a slot for `teaser.mp4` plus a poster image (a screenshot of frame 9).
- Add captions (YouTube auto-captions or a `.vtt`). Most people watch muted.
