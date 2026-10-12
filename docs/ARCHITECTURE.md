# Architecture

How Orch Engine fits together. Read this first if you're new to the code.

## In one paragraph

Orch Engine runs models, MCP tools (Skills) and your own agents, on Docker or Kubernetes, and puts a DLP proxy on the model traffic. On Kubernetes, the `bairav` CLI writes custom resources and an operator turns them into Deployments and Services. On Docker, the same CLI writes a Compose project and runs it. Both targets use the same images and the same DLP engine.

## Pieces

```
            bairav CLI  (cli/)
             │                       │
   --target k8s                --target docker (default)
             │                       │
             ▼                       ▼
   Kubernetes API             ~/.bairav/compose/<stack>/
   Model, Skill, Agent,       docker-compose.yml
   DLPPolicy, AccessGrant           │
             │                      ▼
             ▼                 same containers, one host
   operator (operator/)
   reconciles each kind into Deployments + Services
```

### `cli/`: Go, Cobra

The one binary users install.

- `bairav model`: pull, list, serve, delete, deploy, undeploy, upgrade, rollback, status, discover
- `bairav skill` and `bairav agent`: deploy and manage MCP servers and agents
- `bairav dlp`: run the DLP proxy locally, test a policy, list packs
- `bairav grant`: let another namespace use your model or skill
- `bairav chat`, `bairav ps`, `bairav dashboard`, `bairav visualize`, `bairav version`

Internal packages:

| Package | What it does |
|---------|--------------|
| `internal/hf` | Hugging Face pulls, cache, `discover` |
| `internal/serve` | local serving through llama.cpp's `llama-server` |
| `internal/compose` | renders and runs the Docker target |
| `internal/dlp` | the DLP engine: detectors, packs, proxy and gateway modes, streaming redaction, audit log |
| `internal/dashboard` | the local read-only dashboard (loopback only) |
| `internal/k8sclient`, `internal/openai` | small clients |

### `operator/`: Go, controller-runtime

Five kinds under `orch.bairavs.com/v1alpha1`:

| Kind | What the operator creates |
|------|---------------------------|
| `Model` | Local model: a Deployment of `bairav/serving` (llama-server, pulls the GGUF on start) and a Service. Cloud model (`spec.provider` set): a DLP gateway Deployment that holds the provider key and only forwards inference calls. Upgrades use a surge or recreate strategy, and status reports which model is actually being served. |
| `Skill` | A Deployment and Service for your MCP server image, with health checks. |
| `Agent` | A Deployment and Service for your agent image. Endpoints of the Models and Skills it references are injected as `BAIRAV_MODEL_<NAME>_ENDPOINT` and `BAIRAV_SKILL_<NAME>_ENDPOINT`. The pod doesn't roll until every reference is ready. With `dlpRef` set, a DLP sidecar sits between the agent and its models. |
| `DLPPolicy` | Validates packs and custom rules, and writes custom rules to a ConfigMap the sidecars mount. Runs nothing itself. |
| `AccessGrant` | Lets an Agent in one namespace use a Model or Skill in another. Without it the Agent waits and says which grant it needs. |

The operator has no RBAC access to Secrets (the CLI creates cloud provider keys, pods mount them), runs with a restricted security context, and logs in JSON.

### `images/`

- `serving`: llama.cpp `llama-server` plus an entrypoint that pulls the model.
- `dlp`: the `bairav` binary running `dlp serve`, used as sidecar, enforce-mode proxy and cloud gateway.

## Request path with DLP

1. The agent (or any OpenAI client) sends a chat request to the DLP proxy.
2. The proxy scans every string in the request: built-in detectors (cards with Luhn, SSN, email, secrets for 12+ vendors) plus the enabled packs and custom rules. Text is normalised first, so full-width digits, Indic digits and zero-width characters don't slip through.
3. Each hit is redacted, blocked or only audited, per rule. Block wins.
4. The request goes on to the model. Responses are scanned the same way, streams included.
5. Every decision goes to the audit log, with matched values masked by default.

In enforce mode (`--dlp` on deploy) the model is only reachable through the proxy. For cloud providers the gateway also strips the caller's key, pins the model and allows only inference paths.

## Design choices

- Kubernetes is the API. `kubectl get models` works because Model is a CRD, and everything can live in Git.
- DLP inline, not after the fact. Logging a leak afterwards is auditing; we want to stop it.
- OpenAI's HTTP shape is the contract. Any OpenAI client can talk to any model behind Orch Engine.
- The inference engine is swappable behind `internal/serve.Backend`. Today that's llama.cpp; vLLM is next.
- Packs are YAML, embedded in the binary, so someone who knows a country's ID formats can review and tune them without writing Go.

## Not in scope yet

- Training or fine-tuning
- RAG pipelines and vector stores
- Multi-cluster
- A shared in-cluster dashboard (the dashboard runs on your machine with your kubeconfig)

## Versioning

CRDs stay at `v1alpha1` until three releases ship without breaking changes. The CLI and images share one semver tag.

## Read next

- [`CODING_STANDARDS.md`](CODING_STANDARDS.md) for style rules
- [`HISTORY.md`](../HISTORY.md) for how the project was built
