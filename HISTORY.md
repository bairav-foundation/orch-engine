# How Orch Engine was built

Orch Engine was built by one engineer from April to October 2026. It was developed in a private monorepo with the BAIRAVS website and published here as a clean repository for the public launch, so the git history in this repo starts at the launch commit. This page is the record of how it got here, taken from the build log kept along the way.

| Date | Milestone |
|------|-----------|
| 2026-04-18 | Day 0. Scope research and decision freeze, repo scaffold, Go workspace and the Cobra CLI skeleton. |
| 2026-04-27 | Week 1. Hugging Face model puller, `bairav model serve` on llama.cpp, `bairav ps`, `bairav chat`. |
| 2026-06-08 | The `Model` CRD and operator, a k3d dev loop, the Docker Compose target, and the first prompt DLP proxy. |
| 2026-07-25 | DLP end to end: YAML policy rules with per-rule actions, block-wins semantics, enforce mode that can't be bypassed on the host, Verhoeff-checked Aadhaar, India preset pack. |
| 2026-08-28 | Built-in DLP packs (`global-safe`), UK NHS Mod-11, `bairav dlp packs` and checksum helpers. |
| 2026-09-14 | The `Skill` CRD: MCP servers as first-class Kubernetes resources. |
| 2026-09-19 | The `Agent` CRD: agents wired to models and skills by reference, so the three form one graph. |
| 2026-09-27 | Dependency-loss handling and cascades, `bairav ps` graph view, `bairav visualize`, the `DLPPolicy` CRD with DLP sidecars on agents, response-side and streaming redaction. Docker parity for the whole graph. |
| 2026-10-01 | CRD drift test in CI, unified `--target`, cross-namespace references with the `AccessGrant` CRD. |
| 2026-10-02 | `bairav dashboard`, the demo, and the website rebuild. |
| 2026-10-03 | `bairav model upgrade` / `rollback` with surge rollouts, smoke tests and automatic rollback; `model status` and `model discover`; cloud models (OpenAI, Anthropic, OpenAI-compatible) behind a DLP gateway; a full security audit. |
| 2026-10-05 | An external teardown was vetted and every bypass it found fixed: every string is scanned, routes fail closed, Unicode folding, credential detection, and context rules that cut Aadhaar/NHS false positives from ~10% to under 1%. |
| 2026-10-19 | Public launch, v0.2.0. |
