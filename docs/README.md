# Docs

Commands use `bairav` from the installer (`curl -fsSL https://bairavs.com/install.sh | sh`), with images from `ghcr.io/bairav-foundation`. Building from source instead? Run `make rebuild-all` and use `bin/bairav`; it uses locally built `bairav/*:laptop` images.

## Guides

| Guide | |
|-------|--|
| [`quickstart-model.md`](quickstart-model.md) | deploy a model on Docker or Kubernetes |
| [`quickstart-model-upgrade.md`](quickstart-model-upgrade.md) | upgrade and roll back a model |
| [`quickstart-cloud-models.md`](quickstart-cloud-models.md) | OpenAI, Anthropic and OpenAI-compatible providers behind the DLP gateway |
| [`quickstart-skill.md`](quickstart-skill.md) | run an MCP server as a Skill, with DLP on tool calls; remote MCP servers |
| [`quickstart-agent.md`](quickstart-agent.md) | Model + Skill + Agent as one graph |
| [`quickstart-docker-graph.md`](quickstart-docker-graph.md) | the same graph on Docker only |
| [`quickstart-k8s.md`](quickstart-k8s.md) | local Kubernetes with k3d |
| [`quickstart-dlp.md`](quickstart-dlp.md) | the DLP proxy, packs and custom rules |
| [`quickstart-dlp-on-agent.md`](quickstart-dlp-on-agent.md) | DLP as a sidecar on an Agent |
| [`how-dlp-works.md`](how-dlp-works.md) | what DLP detects, how it scans, and its limits |
| [`quickstart-litellm.md`](quickstart-litellm.md) | BAIRAVS DLP as a LiteLLM guardrail |
| [`quickstart-vllm.md`](quickstart-vllm.md) | serve on a GPU node with vLLM (experimental) |
| [`quickstart-eval.md`](quickstart-eval.md) | check a model's answers with a YAML suite, compare two models, gate upgrades |
| [`quickstart-cross-namespace.md`](quickstart-cross-namespace.md) | sharing models across teams with grants |
| [`quickstart-dashboard.md`](quickstart-dashboard.md) | the local dashboard |
| [`production.md`](production.md) | running in production: checklist, HA, egress, MCP tool pinning, Pod security, support |
| [`verify-release.md`](verify-release.md) | check signatures, checksums, SBOMs and provenance |

## For contributors

- [`ARCHITECTURE.md`](ARCHITECTURE.md): how the pieces fit. Start here.
- [`CODING_STANDARDS.md`](CODING_STANDARDS.md): style rules, read before your first PR.
- [`HISTORY.md`](../HISTORY.md): how the project was built.
