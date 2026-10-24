# Contributing to BAIRAVS Orch Engine

Thanks for thinking about contributing. This doc covers the local dev loop, coding conventions, and the PR flow.

## Prerequisites

- Go 1.22 or later
- Docker (Docker Desktop, Colima, or Rancher Desktop)
- Optional (for K8s work): `kubectl`, `k3d` or `kind`, `helm`, `kubebuilder`
- Optional (for the mcp-echo example): Python 3.11+

## Get the code running

```bash
git clone https://github.com/bairav-foundation/orch-engine
cd orch-engine
make deps      # go mod tidy across all Go modules
make test      # unit tests — should be green on a fresh clone
make build     # builds all binaries into ./bin/
./bin/bairav version
```

If `make test` is not green on a fresh clone, that's a bug. Open an issue.

### Local images and a local cluster

Release binaries pull images from `ghcr.io/bairav-foundation`. A source build uses local images instead:

```bash
make image-serving               # bairav/serving:laptop, for --target docker
make dev                         # k3d cluster "bairavs" with locally built serving + operator images
make dev-down                    # delete it
```

`make dev` is idempotent: it creates the cluster if needed, builds and imports `bairav/serving:laptop` and `bairav/operator:laptop` (no registry), installs the CRDs, RBAC and operator, waits for it to be available, then tails its logs.

## Repo layout

```
orch-engine/
├── cli/          Go — the `bairav` CLI (also contains the DLP engine: cli/internal/dlp)
├── operator/     Go — Kubernetes operator and CRDs (Model, Skill, Agent, DLPPolicy, AccessGrant)
├── images/       Container images: serving (llama.cpp), dlp
├── examples/     Reference MCP skill (mcp-echo) and DLP policies
├── scripts/      CI and end-to-end smoke tests
└── docs/         Quickstarts, architecture, coding standards
```

## Coding standards

See [`docs/CODING_STANDARDS.md`](./docs/CODING_STANDARDS.md) for full details. Summary:

- **Go:** `gofmt`, `goimports`, `golangci-lint` — all must pass before merge.
- **Python:** `ruff format`, `ruff check`, `mypy` — all must pass.
- **Commits:** Conventional Commits (`feat:`, `fix:`, `docs:`, `chore:`, `refactor:`, `test:`).
- **Logs:** Structured JSON only. No printf-style logs outside CLI user-facing output.
- **Errors:** Wrap with context (`fmt.Errorf("doing X: %w", err)`). No silent failures.

## PR flow

1. Branch from `main`: `git checkout -b feat/short-description`.
2. Make changes. Keep PRs under ~400 lines diff when possible.
   **New functionality and bug fixes come with tests:** unit tests for the change, a regression test for a bug, and for anything that changes what runs in a cluster, a check in one of the smoke tests (`scripts/smoke-*.sh`). PRs without tests for new behaviour aren't merged.
3. Run `make ci` locally before pushing (formatting, vet, lint, tests with the race detector, build).
4. Open PR against `main`. CI must be green before merge.
5. Squash-merge to keep history clean.

## Staying aligned with the plan

Work is tracked in GitHub issues. Every PR should reference the issue it closes (e.g., `Closes #42`).

## Questions

Open a [discussion](https://github.com/bairav-foundation/orch-engine/discussions) or an issue, or mention @MohanRajRavi. Security issues: see [SECURITY.md](SECURITY.md), not the issue tracker.
