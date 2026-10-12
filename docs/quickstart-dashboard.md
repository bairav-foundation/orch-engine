# Quickstart: `bairav dashboard`

A local, read-only web console for everything Orch Engine runs: Models, Skills, Agents, DLP policies, AccessGrants, and live DLP decisions, across both the k8s and docker targets.

```bash
bin/bairav dashboard
# bairav dashboard v0.1.0-alpha (live, read-only)
#   open: http://127.0.0.1:7777
```

## What you see

- **Graph.** Models on the left, Agents in the middle, Skills on the right. A solid edge means the ref is bound; a dashed red edge means it's declared but not resolved (missing, pending, or no AccessGrant). 🛡 marks an Agent with a DLP sidecar; ⇄ marks a cross-namespace ref. Click any node for its details. Waiting Agents show the exact reason, including the `bairav grant ...` command when a grant is missing.
- **Stats.** Counts and how many of each are ready, how many Agents are DLP-protected, and how many DLP events were redacted or blocked.
- **DLP activity.** The latest decisions from every DLP sidecar (prompt-side and response-side labels, action, latency). Read from the sidecars' audit stream on each refresh.
- **Tables.** Agents, Models, Skills, DLP policies and AccessGrants, with the same data as `bairav ps` / `bairav grants`.

The page refreshes every 3 seconds. A missing cluster or docker daemon shows up as a warning on the page, not an error.

## How it works (and why it's safe)

- The CLI binary serves one embedded HTML file and one JSON endpoint (`GET /api/snapshot`). There's nothing to install and no server to deploy.
- It reads with **your** kubeconfig and the local docker daemon, so it can see exactly what you can see and nothing more. There's no login because there's nothing new to log in to.
- It's **read-only**: GET only, with POST etc. rejected. To change anything, use the CLI.
- It binds to `127.0.0.1` by default. `--listen 0.0.0.0:7777` works but prints a warning: anyone who can reach that port sees what your kubeconfig sees.
- The page loads nothing from the internet (no CDN fonts or scripts), so it works offline and air-gapped. It's protected by a strict Content-Security-Policy.

## Demo mode

```bash
bin/bairav dashboard --demo                 # scripted walkthrough, no cluster needed
bin/bairav dashboard --export-demo ./site   # writes ./site/index.html: one self-contained file
```

The demo plays nine captioned steps: a model is deployed, Skills are added, an Agent waits for an AccessGrant and then binds, DLP redacts and blocks. It uses the same UI with recorded data. The exported file is what `bairavs.com/orch-engine/demo` hosts, and it's the backdrop for the demo video.

To change the story, edit `scripts/gen-dashboard-demo.py` and run it. It rewrites `cli/internal/dashboard/assets/demo.json`, which is embedded at build time.

## Flags

| Flag | Default | Meaning |
|------|---------|---------|
| `--listen` | `127.0.0.1:7777` | address to serve on |
| `--demo` | off | scripted demo instead of live data |
| `--export-demo DIR` | | write the demo as `DIR/index.html` and exit |
| `--no-k8s` / `--no-docker` | off | skip a target |

## Not yet

- Actions (deploy / grant / undeploy buttons). The dashboard is read-only on purpose until the read view has been used in anger.
- DLP activity for docker-target Agents. Use `docker logs -f bairav-agent-<name>-dlp-<model>`.
- An in-cluster, shareable dashboard (that needs auth and RBAC); it's planned for v1.x.
