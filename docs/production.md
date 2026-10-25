# Running in production

What to set up before Orch Engine carries real traffic, what it already does for you, and what it doesn't do yet. Orch Engine is **alpha**: read [Versions and support](#versions-and-support) first.

## Checklist

| | What | How |
|---|---|---|
| ☐ | Kubernetes 1.28 or later | The CRDs use CEL validation rules; older API servers reject them |
| ☐ | Install from a verified release | [Verify what you install](verify-release.md); deploy images by digest |
| ☐ | Operator HA | On by default from v0.2.1: two replicas, leader election, a PodDisruptionBudget |
| ☐ | A CNI that enforces NetworkPolicy | Calico, Cilium, or k3s' default. Without it, the policies below are created but do nothing |
| ☐ | DLP inside every shared Model | `bairav model deploy ... --dlp-ref <policy>`: the model then listens on localhost only, so no caller skips DLP |
| ☐ | DLP and tool pinning on every Skill | `--dlp-ref` on image Skills (remote Skills always have the proxy), then `bairav skill approve <name>` |
| ☐ | Pinned models | Default from v0.2.1: `model deploy` pins the commit and file SHA-256. Don't use `--no-pin` in production |
| ☐ | Restricted egress for Agents and Skills | `--egress restricted` plus `--egress-allow` for what they really need |
| ☐ | Cross-team access by grant only | `bairav grant`; review grants like you review IAM |
| ☐ | Audit log shipped somewhere you keep | DLP containers write JSON lines to stdout: collect them with your log pipeline |
| ☐ | Your own images run as non-root | See [Pod security](#pod-security) |

## Operator

From v0.2.1 the operator runs **two replicas with leader election**: one reconciles, the other takes over within seconds if the leader's Pod or node fails (`kubectl -n bairav-system get lease bairav-operator-leader` shows the current leader). They prefer different nodes, and a PodDisruptionBudget (`minAvailable: 1`) keeps one running during node drains.

If the operator is down, nothing that's already running stops: Models, Skills and Agents are ordinary Deployments and Services. You just can't deploy or change anything until it's back.

Sizing: each operator replica requests 100m CPU and 128 MiB and is limited to 256 MiB, which is plenty for hundreds of resources. Its metrics are on port 8081 (`/metrics`, Prometheus format) and health on 8082.

## Models

- **Memory.** A GGUF model needs roughly its file size in RAM, plus the context (KV cache). `bairav model discover` shows a rough figure per model. Set `resources` on the Model to at least that, or the node will evict the Pod under pressure.
- **Upgrades.** `bairav model upgrade` with the default `surge` strategy runs old and new side by side until the new one is Ready, so plan memory for both. Add `--eval suite.yaml` so a worse model is rolled back automatically ([Upgrade a model](quickstart-model-upgrade.md)).
- **Pinning.** `bairav model deploy` resolves the revision to a commit and records the file's SHA-256. The serving container checks the hash on every start: a changed or tampered file is downloaded again or refused. Re-run `model deploy` (or `model upgrade`) to move to a newer version on purpose.
- **Restarts re-download.** Weights live in the Pod (emptyDir) and are downloaded again after a restart. A persistent cache is planned; until then, budget the download time and Hugging Face bandwidth.

## Network

**Restricted egress** (`spec.egress.mode: restricted`, or `--egress restricted` on `agent deploy` / `skill deploy`) gives the workload a NetworkPolicy that allows only:

- DNS (port 53),
- the Pods of its own Models and Skills, in their namespaces,
- the IP ranges you list in `spec.egress.allow` (`--egress-allow 203.0.113.7/32:443`).

Everything else outbound is dropped, so a compromised agent or tool can't send data or stolen credentials anywhere. NetworkPolicy works on IP ranges, not hostnames: for a SaaS API, use its published ranges or send it through an egress proxy and allow the proxy. A remote Skill's gateway needs its MCP server's range in `allow`.

Redeploying without `--egress` keeps the current setting; pass `--egress open` to remove it.

**Inbound**: a Model with `dlpRef` only accepts traffic through its DLP proxy (the model itself listens on localhost). An image Skill with `dlpRef` gets a NetworkPolicy that leaves only the proxy's port open.

## MCP tools

MCP servers describe their tools to the model, and the model follows those descriptions. A server that changes a description after you started using it can steer your agent: "tool poisoning", or a "rug pull" when it happens later.

The MCP proxy (in front of every remote Skill, and every image Skill with a `dlpRef`) fingerprints each tool's name, description and input schema:

- **Without a pin** it trusts the first version it sees and blocks any later change for as long as it runs.
- **With a pin** (`bairav skill approve <name>`) only the approved tools, unchanged, get through; anything new or changed is held back until you review and approve again.

A held-back tool returns a JSON-RPC error (`-32002`) that names it and says how to approve, and every tool list the proxy sees is in its audit log.

## Pod security

What Orch Engine runs itself (the operator, the serving image, DLP proxies and gateways) runs as non-root, with no Linux capabilities, no privilege escalation, a RuntimeDefault seccomp profile, and a read-only root filesystem where the process allows it. Model, Skill and Agent Pods get **no Kubernetes service account token**: none of them call the Kubernetes API, and a token in a compromised Pod is a common way into a cluster.

**Your own images** (the agent, and image Skills' MCP servers) run as you built them. For the Pod Security "restricted" level, build them to run as a non-root user. A `securityContext` field on Agent and Skill is planned.

## Secrets

- Cloud provider keys live in a Secret that only the DLP gateway Pod reads; agents never see them, and the gateway removes any key a caller sends.
- A remote MCP server's token lives in a Secret read only by its gateway; agents can't swap in their own.
- Hugging Face tokens for gated models: `spec.hfTokenSecretRef` on the Model.

The operator itself has no permission to read Secrets: the kubelet resolves them into the Pods that reference them.

## Versions and support

- **Alpha.** The CRDs are `orch.bairavs.com/v1alpha1`. Fields can change between minor versions (0.2 → 0.3); patch releases (0.2.0 → 0.2.1) only add optional fields and fix bugs, so existing resources keep working. Breaking changes are listed in the release notes with the steps to move.
- **Supported versions:** the latest release gets fixes, including security fixes. Upgrade by applying the new `install.yaml` and installing the new CLI. The operator rolls first; workloads restart only when the new version changes their Pod template, and the release notes say when. **v0.2.1 does:** every Model, Skill and Agent Pod restarts once (new security settings), and models download their weights again, so upgrade in a quiet hour.
- **Security reports:** acknowledged within 3 days, fix or plan within 14 days for confirmed issues ([SECURITY.md](../SECURITY.md)).
- **Help:** [GitHub Discussions](https://github.com/bairav-foundation/orch-engine/discussions) for questions, issues for bugs. There's no paid support yet; for a production evaluation, write to hello@bairavs.com.
- **Testing:** every change runs the unit tests, `govulncheck` and lint in CI; every night a real k3d cluster runs the DLP and MCP smoke tests ([nightly workflow](../.github/workflows/nightly.yml)).

## Not there yet

Said plainly, so you can plan around it:

- No persistent model cache (weights re-download after a Pod restart).
- One replica per Model, no autoscaling.
- No `securityContext` override for your own agent and skill containers.
- DLP detects structured identifiers (IDs, card numbers, keys, emails), not names or addresses.
- vLLM support is experimental and not tested in CI (no GPU runner).
- No Helm chart yet: `install.yaml` is plain manifests.
