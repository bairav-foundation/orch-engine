# Architecture

The mental model of BAIRAVS Orch Engine. Read this first if you're new to the codebase.

## One-line summary

A Kubernetes-native control plane that deploys open-source LLMs, executes skills (LangGraph/CrewAI agents) against them, and enforces a DLP policy on every prompt — all self-hostable, single Helm chart.

## The four MVP slices

Everything in this repo implements one of four slices:

1. **LLM Model Deploy** — pull a GGUF/AWQ model from HuggingFace, run it on a GPU (or CPU/Metal) inside the user's cluster, expose it at an OpenAI-compatible endpoint.
2. **Skill Runtime** — execute a Python skill (LangGraph or CrewAI) in a sandboxed container that can call the deployed model.
3. **Prompt DLP** — an inline HTTP proxy that evaluates every prompt against a policy and blocks, masks, or alerts on PII, secrets, and DB snapshot patterns.
4. **Unified K8s-native Control Plane** — a single operator, a single CLI, a single Helm chart — one install, everything reconciles via CRDs.

## Components

```
              ┌─────────────────────────────────────────────────┐
              │                   User (laptop)                  │
              │           kubectl  |  bairav CLI  |  helm       │
              └──────────────────────┬──────────────────────────┘
                                     │ (kube API)
                                     ▼
     ┌───────────────────────────────────────────────────────────┐
     │                  Kubernetes cluster                         │
     │                                                             │
     │   ┌───────────────────┐       ┌────────────────────┐        │
     │   │    Operator       │◀─────▶│   CRDs (etcd)      │        │
     │   │  (controller-     │       │   Model            │        │
     │   │   runtime)        │       │   Skill            │        │
     │   │                   │       │   DLPPolicy        │        │
     │   └─────────┬─────────┘       └────────────────────┘        │
     │             │ reconciles                                     │
     │   ┌─────────▼─────────┐       ┌────────────────────┐        │
     │   │  Inference Pod     │      │  Serving Proxy     │        │
     │   │  (vLLM / Ollama)   │◀─────│  (DLP inline)      │◀─── clients
     │   │   :8000 OpenAI     │      │   :8080 OpenAI     │        │
     │   └────────────────────┘      └──────────┬─────────┘        │
     │                                          │ calls            │
     │                                ┌─────────▼─────────┐         │
     │                                │  DLP Engine       │         │
     │                                │  (pure-Go lib)    │         │
     │                                └───────────────────┘         │
     │                                                              │
     │   ┌────────────────────┐                                     │
     │   │   Skill Runtime    │───── HTTP ──────────┐              │
     │   │   Pod (Python)     │       (via proxy)   │              │
     │   │   CrewAI/LangGraph │◀────────────────────┘              │
     │   └────────────────────┘                                     │
     └─────────────────────────────────────────────────────────────┘
```

### `cli/` — Go / Cobra

The only CLI the user installs. Commands:
- `bairav model {pull,deploy,list}` — model lifecycle (creates `Model` CRs).
- `bairav skill {package,deploy,run,list,logs}` — skill lifecycle (creates `Skill` CRs).
- DLP policies: `DLPPolicy` CRs applied with `kubectl apply`, attached with `bairav agent deploy --dlp-policy`; locally `bairav dlp serve --policy` / `bairav dlp test`.
- `bairav version`

The CLI never talks to pods directly. It only writes CRs into the Kubernetes API; the operator does all the work.

### `operator/` — Go / controller-runtime (kubebuilder)

Reconciles three CRDs:

| CRD | What it creates |
|-----|-----------------|
| `Model` | A Deployment running vLLM or Ollama, a Service, and a readiness probe |
| `Skill` | A Job (on-demand) or Deployment (if `exposeHTTP: true`) running the skill-runtime image with the user's `main.py` mounted |
| `DLPPolicy` | A ConfigMap consumed by every serving-proxy replica; no Deployment of its own |

All CRDs live under the API group `orch.bairavs.com/v1alpha1`.

### `serving-proxy/` — Go HTTP proxy

Sits in front of every inference pod. Implements the OpenAI Chat Completions API on port 8080. On every request:

1. Look up the matching `DLPPolicy` (by namespace → cluster default fallback).
2. Call `dlp-engine.Evaluate(prompt, policy)`.
3. If action is `BLOCK`: return HTTP 403 with a reason.
4. If action is `MASK`: replace matched spans with placeholders, forward the masked prompt.
5. If action is `ALERT`: forward unchanged but emit a Prometheus counter + structured log.
6. Forward to the upstream inference pod, stream response back.

Latency budget: <50ms p95 added over raw inference.

### `dlp-engine/` — Go library, no I/O

Pure functions over strings. Detector taxonomy:

| Category | Examples |
|----------|----------|
| PII | emails, phone numbers, credit cards (Luhn-validated), SSNs |
| Secrets | AWS keys, GCP service-account JSON, GitHub tokens, private keys (PEM), JWT |
| DB snapshots | SQL-looking INSERT/CSV blobs with >N columns × >M rows |

Returns a `Decision` with matched spans, their categories, and the applied action (from the policy).

### `skill-runtime/` — Python 3.11 container

Base image: `python:3.11-slim` + CrewAI + LangGraph + openai + httpx (pinned).
Entry contract: reads `BAIRAV_INPUT` env var (JSON), runs user's `main.py`, captures stdout as the result, returns JSON.
Model endpoint is wired via `OPENAI_API_BASE` → the serving-proxy.

### `helm/` — single Helm chart

`bairavs-orch` installs: operator Deployment + ClusterRole, all three CRDs, serving-proxy Deployment + Service, default `DLPPolicy`, optional ServiceMonitor for Prometheus Operator users.

### `compose/` — Docker Compose for laptops

Same components, single-node, generated from Helm values to prevent drift.

## Request flow: end-to-end skill invocation

When a developer runs `bairav skill run code-reviewer --input '{"diff": "..."}'`:

1. **CLI** packages the input, creates a `Skill` CR with `runOnce: true` and the input payload.
2. **Operator** sees the new `Skill` CR, creates a Kubernetes Job that mounts `main.py` from a ConfigMap, sets `BAIRAV_INPUT=<json>`, sets `OPENAI_API_BASE=http://serving-proxy.<ns>:8080`.
3. **Pod** starts, runs `entrypoint.py` which invokes user's `main.py`.
4. **Skill code** calls the model via `openai.ChatCompletion.create(...)` — which hits the **serving-proxy**.
5. **Serving-proxy** pulls the active `DLPPolicy`, calls `dlp-engine.Evaluate` on the prompt, takes the action.
6. If allowed: proxy forwards to **inference pod**, streams the response back to the skill.
7. **Skill** writes JSON result to stdout.
8. **Entrypoint** captures stdout, writes a structured result to a known file.
9. **Operator** observes Job completion, updates `Skill` status with the result reference.
10. **CLI** polls `Skill` status, prints the result.

## Data flow: DLP on model calls

Same as steps 4–6 above, but triggered by any client calling the model — not just skills. Any OpenAI SDK pointed at the serving-proxy endpoint gets DLP for free.

## Key design decisions

- **Kubernetes is the API.** We don't wrap it, we build on it. `kubectl get models` works out of the box because `Model` is a CRD.
- **DLP inline, not batch.** If it's batch, it's auditing. We want prevention.
- **Go for the control plane, Python only for skill execution.** CrewAI + LangGraph are Python-native — wrapping them via RPC would double the surface area.
- **OpenAI API compatibility is the contract** at the serving-proxy boundary. Any client that speaks OpenAI speaks BAIRAVS.
- **Operator watches cluster-wide by default, scopable to namespaces** via Helm values. Multi-tenancy is namespace-based on-prem; SaaS multi-tenancy is post-MVP.
- **Stateless skills, stateless proxy, stateless DLP.** State lives in etcd (CRDs) and in the inference pod's model weights. No external databases MVP.

## Non-goals (MVP)

- Fine-tuning or training pipelines.
- Vector databases, RAG orchestration, document ingestion.
- Multi-cluster federation.
- A web UI beyond what Prometheus/Grafana provide externally.
- Any cloud-provider adapter — if you have GPU nodes in K8s, it works.

## Versioning

- CRDs ship under `v1alpha1` until we've cut three releases without breaking changes, at which point we promote.
- Container images and the Helm chart follow semver. Tags like `v0.1.0-alpha.1`.
- The CLI follows the same semver as the chart.

## Backend strategy — the inference engine is interchangeable

The serving layer has two boundaries:

- **Stable / customer-facing:** `POST /v1/chat/completions` (and the rest of the OpenAI shape). Skills, agents, and downstream clients only ever see this.
- **Variable / internal:** Which inference engine actually executes the model — `llama-server`, `vLLM`, `Ollama`, `TGI`, `sglang`, MLX. Implementation detail.

The `internal/serve.Backend` interface is the seam. As long as a Backend speaks the OpenAI shape on its bound port, BAIRAVS doesn't care what's behind it.

### Today (Week 1)

- **`llamaserver`** (llama.cpp's `llama-server`) — laptop, CPU/Metal/consumer-GPU. Reads our HF-pulled GGUF directly. One process, one model, one port.

### Roadmap

| Horizon | Backends added | Rationale |
|---------|---------------|-----------|
| Week 1 (now) | llama-server | Reads pulled GGUF, native OpenAI compat, one binary, Metal/CPU/CUDA |
| Post-MVP | vLLM | GPU production — high throughput, batched serving, safetensors |
| Post-MVP | Ollama | For users who already run it; broader catalog story |
| Long-term | sglang, MLX, TGI, anything community-driven | Plugin architecture — registers as a Backend impl |

### What customers install

**They don't install backends.** When we ship containers (Week 4), the right engine is baked into the container for the environment:

| Path | Container | Engine inside |
|------|-----------|---------------|
| Laptop (`docker compose up`) | `bairav/serving:laptop` | llama-server (CPU/Metal) |
| K8s no-GPU (`helm install`) | `bairav/serving:cpu` | llama-server |
| K8s with GPU (`helm install`) | `bairav/serving:gpu-vllm` | vLLM |

The customer brings open-weight models. We bring the runtime. They never type `brew install llama.cpp`.

The Day 3-4 dev loop (`bairav model serve`) shells out to a *locally-installed* `llama-server` because we're in the build phase — that's prereq for us building the product, not for the customer using it.

### What we ARE coupled to

- **GGUF format** for the cheap-quantized-laptop path. GGUF is open-spec, no vendor owns it, used by llama.cpp / Ollama / LM Studio. Mitigated by treating GGUF as one of several formats — when we add vLLM we'll add safetensors pulling alongside.
- **OpenAI HTTP shape** as the API contract. This is the closest thing to a de-facto standard for LLM serving in 2026 and is honored by every backend on our roadmap.

Neither of these is vendor lock-in — both are open standards.

### The K8s analogy

Long-term: **BAIRAVS is to inference engines what Kubernetes is to container runtimes.** containerd / CRI-O / runc — K8s doesn't care, you don't care. Same shape here: llama.cpp / vLLM / MLX / sglang — BAIRAVS doesn't care, your skill code doesn't care. One stable API, swappable execution.

---

## Where to read next

- `docs/CODING_STANDARDS.md` for the style rules.
- [`HISTORY.md`](../HISTORY.md) for how the project was built, milestone by milestone.
