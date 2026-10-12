# Quickstart — deploy an MCP server as a Skill

A **Skill** in BAIRAVS is any MCP server (Model Context Protocol) packaged as a container the user provides. BAIRAVS doesn't ship MCP servers itself — the user does, in whatever language they want. What BAIRAVS provides is the *substrate*: `bairav skill deploy <image>` turns an MCP server image into a K8s-native, discoverable, governable resource.

Same operator pattern as Models. Same CLI shape. Different content.

---

## Prerequisites

- k3d cluster with the operator running (`make dev` if you haven't already).
- An MCP server image. This walkthrough uses [`bairav/mcp-echo`](../examples/mcp-echo/README.md) — a real, buildable ~55MB reference server that speaks MCP over SSE. Build it with `docker build -t bairav/mcp-echo:v1 examples/mcp-echo` (see the README for pushing to the k3d dev registry). Substitute your own MCP image for real deployments.

---

## Deploy

```bash
bin/bairav skill deploy k3d-bairavs-registry:5000/bairav/mcp-echo:v1 --name echo --target k8s
```

Output:

```
Context:    k3d-bairavs
Namespace:  default
Name:       echo
Image:      k3d-bairavs-registry:5000/bairav/mcp-echo:v1
Transport:  sse
Port:       3000

Namespace "default" does not need to be created.
Creating Skill default/echo ...
✓ Created
Waiting for Skill Ready (timeout 3m0s) ...
  phase=Pending endpoint=—
  phase=Deploying endpoint=—
  phase=Ready endpoint=http://echo.default.svc.cluster.local:3000
✓ Skill default/echo is Ready — endpoint: http://echo.default.svc.cluster.local:3000
```

The Skill CR is created, the operator reconciles it into a Deployment + Service, and the CLI polls `.status.phase` until Ready.

---

## Inspect

```bash
bin/bairav skill list --target k8s                    # skills in `default`
bin/bairav skill list --all-namespaces --target k8s   # every namespace
kubectl get skills                       # kubectl also works — Skills are first-class CRs
kubectl describe skill filesystem        # full spec + conditions + endpoint
```

---

## Talk to it (from inside the cluster)

An Agent, or any other Pod in the cluster, can hit the Skill at the endpoint printed above:

```bash
kubectl run --rm -it debug --image=curlimages/curl -- \
  curl -N http://filesystem.default.svc.cluster.local:3000/sse
```

For local debugging, `kubectl port-forward` brings the endpoint to your laptop:

```bash
kubectl port-forward svc/filesystem 3000:3000
curl -N http://127.0.0.1:3000/sse
```

---

## Custom knobs

```bash
bin/bairav skill deploy ghcr.io/myorg/customer-lookup:v2 --target k8s \
  --name customer-api \
  --namespace bairav-skills \
  --transport http \
  --port 8000 \
  --health-path /healthz \
  --timeout 5m
```

Flags:

| Flag | Default | Purpose |
|------|---------|---------|
| `--name`, `-n` | derived from image | Explicit CR name (must be DNS-1123 safe) |
| `--namespace` | `default` | K8s namespace; auto-created if missing |
| `--transport` | `sse` | `sse` or `http` — the MCP transport the server speaks |
| `--port` | `3000` | Port the MCP server listens on inside the container |
| `--health-path` | `/health` | Path used by K8s readiness/liveness probes |
| `--wait` | `true` | Poll until `status.phase = Ready` before returning |
| `--timeout` | `3m` | How long `--wait` waits before giving up |

**Naming derivation:** `bairav/mcp-echo:v1` → `mcp-echo`. `ghcr.io/myorg/customer-lookup:latest` → `customer-lookup`. `registry.example.com:5000/foo/bar@sha256:...` → `bar`. Strips registry + org + tag/digest, DNS-1123-sanitises the tail. Override with `--name` when the derivation isn't what you want (this walkthrough uses `--name echo` for a cleaner label).

---

## Tear down

```bash
bin/bairav skill undeploy filesystem --target k8s
# or force (skip confirm):
bin/bairav skill undeploy filesystem -f --target k8s
```

Owner references on the underlying Deployment + Service cascade the delete — no orphans.

---

## What Skills are and aren't (v1)

**Are:**
- Long-running MCP servers deployed as K8s Deployments + Services.
- First-class CRs (`skills.orch.bairavs.com`) so `kubectl` sees them alongside your other workloads.
- Health-checked, restarted automatically, cascading-deleted.

**Aren't (yet — deferred to later):**
- **stdio-transport MCP servers** — v1 supports HTTP + SSE only. stdio needs a different runtime shape (sidecar in the calling Agent's Pod, or a stdio→SSE adapter). Land alongside the Agent CRD in Week 4.
- **Auto-discovery / registry** — an Agent CR that references Skills gets them wired automatically. Today, callers use the in-cluster DNS name directly.
- **Per-Skill DLP attachment** — Skills currently talk to whatever's on the other end of their tools. Wiring DLP into their outbound calls is Week 4 territory (Agent CRD manages the connection graph).
- **Docker Compose target**: shipped Day 20 as `bairav skill deploy <image> --target docker`. See [`quickstart-docker-graph.md`](./quickstart-docker-graph.md).

---

## Cross-reference

- [`quickstart-model.md`](./quickstart-model.md) — deploy an LLM. Same operator pattern.
- [`quickstart-dlp.md`](./quickstart-dlp.md) — the DLP proxy that will front Agents (and, in Week 4, Skills accessed by Agents).
