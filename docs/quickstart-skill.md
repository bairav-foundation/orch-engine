# Quickstart — deploy an MCP server as a Skill

A **Skill** is an MCP server (Model Context Protocol) that Orch Engine runs for you: a container image you provide, in any language, or a remote server's URL. `bairav skill deploy <image>` runs it on Docker or Kubernetes, gives it a name agents can refer to, and can put DLP on its tool calls.

Same operator pattern as Models. Same CLI shape. Different content.

---

## Prerequisites

- A cluster with the operator installed ([quickstart-k8s.md](quickstart-k8s.md)), or Docker for `--target docker`.
- An MCP server image. This walkthrough uses [`mcp-echo`](../examples/mcp-echo/README.md), a small reference server that speaks MCP over SSE, published as `ghcr.io/bairav-foundation/mcp-echo`. Use your own MCP image for real work.

---

## Deploy

```bash
bairav skill deploy ghcr.io/bairav-foundation/mcp-echo:v0.2.0 --name echo --target k8s
```

Output:

```
Context:    k3d-bairavs
Namespace:  default
Name:       echo
Image:      ghcr.io/bairav-foundation/mcp-echo:v0.2.0
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
bairav skill list --target k8s                    # skills in `default`
bairav skill list --all-namespaces --target k8s   # every namespace
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
bairav skill deploy ghcr.io/myorg/customer-lookup:v2 --target k8s \
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

## DLP on tool calls

An agent's tool calls carry whatever the model decided to send, often text it read from a user or a document. Tool results go straight back into the model's context. `--dlp-ref` puts a DLP policy on both:

```bash
kubectl apply -f - <<'EOF'
apiVersion: orch.bairavs.com/v1alpha1
kind: DLPPolicy
metadata: {name: tools-guard}
spec:
  packs: [global-safe, india]
  defaultAction: redact
  direction: both
EOF
bairav skill deploy ghcr.io/bairav-foundation/mcp-echo:v0.2.0 --name echo --target k8s --dlp-ref tools-guard
```

What the operator does:

- Adds a `dlp` container to the Skill Pod (`bairav dlp serve --mcp`). The Service keeps its port and now points at the proxy, so Agents need no changes.
- Adds a NetworkPolicy, `echo-dlp-only`, that admits traffic to the Pod on the proxy's port only. The MCP server's own port is closed to the rest of the cluster. This needs a CNI that enforces NetworkPolicy (k3s and k3d do, as do Calico and Cilium).
- Fails closed: no Deployment until the policy exists and is valid. `kubectl get skill echo` shows a `DLPEnforced` condition either way.

The proxy speaks both MCP HTTP transports (SSE and streamable HTTP). It scans every string in each JSON-RPC message: `tools/call` arguments, prompt arguments and resource reads on the way in, results on the way out, whether they come back as JSON or as SSE events. A message with a hit under a `block` rule never reaches the server; the client gets a JSON-RPC error with code `-32001`. Bodies that aren't JSON are refused.

On Docker:

```bash
bairav skill deploy ghcr.io/bairav-foundation/mcp-echo:v0.2.0 --name echo --dlp --dlp-pack global-safe,india
```

The server then runs on the stack's private network only, with the proxy as the one way in.

## Remote MCP servers

Pass a URL instead of an image to use an MCP server someone else runs:

```bash
kubectl create secret generic github-mcp --from-literal=token=<token>
bairav skill deploy https://api.example.com/mcp --name github --transport http \
  --target k8s --auth-secret github-mcp:token --dlp-ref tools-guard
```

The operator runs a small gateway Pod in front of it. Agents call `http://github.<ns>.svc.cluster.local:3000/mcp` as with any Skill, and the gateway:

- adds the token from the Secret to every upstream request and drops whatever credential the caller sent, so agents never hold it
- enforces the DLP policy on arguments and results; without `--dlp-ref` it still audits every call (audit only, built-in detectors)
- keeps the URL's path in the Skill's endpoint, so callers use the same route

On Docker the token comes from `$BAIRAV_MCP_TOKEN` when you deploy (compose reads it at start-up; it isn't written to the stack's files):

```bash
BAIRAV_MCP_TOKEN=<token> bairav skill deploy https://api.example.com/mcp --name github --transport http
```

The gateway can't stop an agent from calling the remote URL directly. Pair it with an egress NetworkPolicy on your Agents if that matters.

## Pin the tools (tool poisoning, rug pulls)

The model reads each tool's description as an instruction, so a server that changes a description later can steer your agent. The MCP proxy (every remote Skill, and image Skills with `--dlp-ref`) fingerprints each tool's name, description and input schema. Without a pin it trusts the first version it sees and blocks later changes; with a pin, only the tools you approved get through, unchanged.

After your agent (or any MCP client) has listed the tools once:

```bash
bairav skill approve crm --namespace team-a
```

```
Tools offered by team-a/crm now:

TOOL           CHANGE  DESCRIPTION
create_ticket  -       Open a support ticket.
search         -       Search the knowledge base.

Read the descriptions above: the model treats them as instructions.
Approving pins these 2 tools for crm.
Type 'yes' to confirm: yes
✓ Pinned 2 tools on team-a/crm. The proxy restarts with the new list; anything else is held back.
```

From then on a new or changed tool returns JSON-RPC error `-32002` naming it, `tools/call` for it is refused, and the audit log has an `mcp_tools_blocked` entry. Run `approve` again to review the change and accept it. `--yes` skips the prompt (CI).

## Limit outbound traffic

```bash
bairav skill deploy ghcr.io/bairav-foundation/mcp-echo:v0.2.0 --target k8s --namespace team-a \
  --egress restricted --egress-allow 203.0.113.7/32:443
```

The Skill's Pods can then reach only DNS and the listed ranges (a remote Skill's gateway needs its MCP server's range). Needs a CNI that enforces NetworkPolicy. More in [Running in production](production.md#network).

## Tear down

```bash
bairav skill undeploy filesystem --target k8s
# or force (skip confirm):
bairav skill undeploy filesystem -f --target k8s
```

Owner references on the underlying Deployment + Service cascade the delete — no orphans.

---

## What Skills are and aren't (v1)

**Are:**
- Long-running MCP servers deployed as K8s Deployments + Services.
- First-class CRs (`skills.orch.bairavs.com`) so `kubectl` sees them alongside your other workloads.
- Health-checked, restarted automatically, cascading-deleted.

**Aren't, yet:**
- **stdio-transport MCP servers** — v1 supports HTTP + SSE only. stdio needs a different runtime shape (sidecar in the calling Agent's Pod, or a stdio→SSE adapter).
- **DLP on what a tool does with its own outbound calls.** The proxy sees what goes into and comes out of the MCP server, not the server's own calls to third-party APIs.

---

## Cross-reference

- [`quickstart-model.md`](./quickstart-model.md) — deploy an LLM. Same operator pattern.
- [`quickstart-dlp.md`](./quickstart-dlp.md) — the standalone DLP proxy.
