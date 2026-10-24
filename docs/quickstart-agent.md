# Quickstart — the full graph: Agent + Model + Skill

Model, Skill and Agent together as one deployable graph. Same operator, same cluster, same CLI. **You do not write any agent framework code in BAIRAVS.** You bring your own container (LangGraph, AutoGen, the Anthropic SDK, plain Go, anything); BAIRAVS runs it and wires it up.

---

## The mental model

```
┌──────────────────────────────────────────────────────────┐
│                       Agent (CR)                         │
│  image: ghcr.io/you/your-agent:v1     ← your framework   │
│  modelRefs: [qwen2-5-0-5b]            ← "I need this"    │
│  skillRefs: [echo, search]            ← "and these"      │
└─────────────┬──────────────────────────┬─────────────────┘
              │                          │
     ┌────────▼────────┐        ┌────────▼──────────────┐
     │   Model (CR)    │        │   Skill (CR)          │
     │   Qwen 0.5B     │        │   MCP echo            │
     │   OpenAI shim   │        │   MCP search          │
     └─────────────────┘        └───────────────────────┘
     ← BAIRAVS runs it →     ← BAIRAVS runs them →
```

The operator resolves every ref, waits for each dependency to be **Ready**, then injects endpoint env vars and rolls the Agent's Pod. Your container ends up with a clean environment:

```
BAIRAV_MODEL_QWEN2_5_0_5B_ENDPOINT   = http://qwen2-5-0-5b.default.svc.cluster.local:8080/v1
BAIRAV_SKILL_ECHO_ENDPOINT           = http://echo.default.svc.cluster.local:3000
BAIRAV_SKILL_ECHO_TRANSPORT          = sse
BAIRAV_SKILL_SEARCH_ENDPOINT         = http://search.default.svc.cluster.local:3000
BAIRAV_SKILL_SEARCH_TRANSPORT        = sse
```

Your agent code reads those. No endpoints are hardcoded; the graph is declared in the CRs.

---

## Full walkthrough

**1. Bring up the Model.**

```bash
bairav model deploy Qwen/Qwen2.5-0.5B-Instruct-GGUF --target k8s
# wait for status.phase = Ready
```

**2. Bring up the Skills.**

```bash
bairav skill deploy ghcr.io/bairav-foundation/mcp-echo:v0.2.0 --name echo --target k8s
bairav skill deploy ghcr.io/bairav-foundation/mcp-echo:v0.2.0 --name search --target k8s
# both Ready — same demo image, two Skill CRs. See examples/mcp-echo/README.md for the image.
```

**3. Deploy the Agent that references them.**

```bash
bairav agent deploy ghcr.io/myorg/support-agent:v1 --target k8s \
  --model qwen2-5-0-5b \
  --skill echo \
  --skill search
```

Output:

```
Context:    k3d-bairavs
Namespace:  default
Name:       support-agent
Image:      ghcr.io/myorg/support-agent:v1
Port:       8000
Models:     qwen2-5-0-5b
Skills:     echo, search

Creating Agent default/support-agent ...
✓ Created
Waiting for Agent Ready (timeout 5m0s) ...
  phase=Pending endpoint=—
  phase=Deploying endpoint=—
  phase=Ready endpoint=http://support-agent.default.svc.cluster.local:8000
✓ Agent default/support-agent is Ready — endpoint: http://support-agent.default.svc.cluster.local:8000
  bound models: qwen2-5-0-5b
  bound skills: echo, search
```

If any dependency isn't Ready yet, you'll see a `WaitingForDeps` phase with the specific ref that's blocking:

```
  phase=WaitingForDeps endpoint=—  (pending Models: qwen2-5-0-5b)
```

The operator requeues every 5 seconds, so as soon as the Model reaches Ready the Agent phase moves forward automatically.

**4. Inspect the whole graph.**

```bash
bairav model list && bairav skill list --target k8s && bairav agent list --target k8s
```

Or with `kubectl` — all three are first-class CRs under the same API group:

```bash
kubectl get models,skills,agents
```

**5. Visualise the whole graph.**

Once the three CRs are up, `bairav visualize` prints a Mermaid diagram of the whole thing — nodes for every Model / Skill / Agent, edges for every ref (solid when bound, dashed when a ref hasn't resolved yet):

```bash
bairav visualize                     # mermaid on stdout
bairav visualize --format dot        # Graphviz, e.g. | dot -Tpng > graph.png
```

Paste the Mermaid output into [mermaid.live](https://mermaid.live), a GitHub markdown block, or Obsidian, and the graph draws itself. Useful for showing how the Model, Skills and Agent connect without switching between `kubectl get` commands.

**6. Talk to the agent.**

The Agent's `Service` exposes whatever HTTP surface your container defines on `port: 8000` (or whatever you set). Port-forward from your laptop or hit it from another Pod:

```bash
kubectl port-forward svc/support-agent 8000:8000
curl http://127.0.0.1:8000/chat -d '{"message":"look up customer 4477291"}'
# your agent reads BAIRAV_MODEL_*_ENDPOINT + BAIRAV_SKILL_*_ENDPOINT
# and orchestrates the actual work
```

**7. Tear down.**

```bash
bairav agent undeploy support-agent -f --target k8s
# Owner refs cascade the underlying Deployment + Service. Model + Skills are untouched.
```

---

## The env-var contract, precisely

For every `modelRefs[].name = X`:

```
BAIRAV_MODEL_<UPPER_UNDERSCORED_X>_ENDPOINT   = <Model.status.endpoint>
```

For every `skillRefs[].name = Y`:

```
BAIRAV_SKILL_<UPPER_UNDERSCORED_Y>_ENDPOINT   = <Skill.status.endpoint>
BAIRAV_SKILL_<UPPER_UNDERSCORED_Y>_TRANSPORT  = "sse" or "http"
```

Name transformation is uppercase + hyphen→underscore. `qwen2-5-0-5b` → `QWEN2_5_0_5B`. `customer-lookup` → `CUSTOMER_LOOKUP`.

Your Env from `spec.env` comes through unchanged (typically `ANTHROPIC_API_KEY`, framework config, etc.). The operator-injected `BAIRAV_*` env vars never collide with user env — they own the `BAIRAV_` namespace.

---

## Limit where the agent can connect

```bash
bairav agent deploy ghcr.io/team-a/bot:v1 --target k8s --namespace team-a \
  --model shared-models/qwen --skill tickets --egress restricted
```

With `--egress restricted` the agent's Pods can reach DNS, their own Models and Skills, and the ranges in `--egress-allow` only, so a compromised agent can't send data or credentials anywhere else. Redeploying without `--egress` keeps it; `--egress open` removes it. Details in [Running in production](production.md#network).

## What Agents are and aren't (today)

**Are:**
- First-class CRs (`agents.orch.bairavs.com`) alongside Models and Skills.
- Declaratively wired: the CR names its dependencies; the operator resolves them.
- Dependency-safe: no Agent Pod starts with unresolved endpoint env vars.
- Health-checked, cascade-deleted, kubectl-native.
- DLP-covered: model traffic through the agent sidecar or the model's own `dlpRef` ([DLP on an Agent](./quickstart-dlp-on-agent.md)), tool calls through the Skill's `dlpRef` ([DLP on tool calls](./quickstart-skill.md#dlp-on-tool-calls)).

**Aren't, yet:**
- **Rate limits on shared Models.** Cross-namespace refs work (`--model team-a/qwen` plus `bairav grant`; see [`quickstart-cross-namespace.md`](./quickstart-cross-namespace.md)), but a granted team can still overload a shared Model. Per-consumer limits on the grant come later.
- **Auto-scale on dep loss.** When a referenced Model/Skill disappears, the Agent's Pod stays running (K8s convention: reflect state, don't enforce corrective action). Phase flips to `WaitingForDeps` with a `Degraded=True` Condition naming what vanished; `kubectl describe agent` explains the exact story. For explicit cleanup use `bairav model undeploy X --cascade-agents` (or same on `skill undeploy`) — the CLI lists dependents up front so surprise cascades are impossible.
- **Auto-scaling.** Replicas=1 for now. HPA integration comes later once we know what the right per-agent metric is (probably queue depth, but per-framework).
- **Live dependency tracking on Docker.** `--target docker` works (see [`quickstart-docker-graph.md`](./quickstart-docker-graph.md)), but refs are resolved once at deploy time. There's no controller on the laptop, so no WaitingForDeps.

---

## Cross-reference

- [`quickstart-model.md`](./quickstart-model.md) — deploy a Model. The first layer of the graph.
- [`quickstart-skill.md`](./quickstart-skill.md) — deploy Skills (MCP servers). The tools your agent calls.
- [`quickstart-dlp.md`](./quickstart-dlp.md) — the standalone DLP proxy.
