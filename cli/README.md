# `cli/` — the `bairav` CLI

This module is the primary user-facing entry point. Everything a user can do with BAIRAVS Orch Engine is invocable via `bairav <resource> <action>`.

## Status

Day 0 scaffold complete. Cobra root + subcommand stubs (`model`, `skill`, `policy`, `version`) registered. Each stub returns a clear "not yet implemented" error pointing to the sprint day it gets wired up.

## Build and run

```bash
# From orch-engine/
make build-cli
./bin/bairav version
./bin/bairav --help

# Directly from this module:
go run ./ version
go test ./...
```

## Command plan

| Command | Ships | Notes |
|---------|-------|-------|
| `bairav version` | Day 0 ✅ | Prints the build-time version string. |
| `bairav model pull/deploy/list` | Week 1 | HF puller, K8s deploy, local Docker fallback. |
| `bairav policy apply/list/describe/delete` | Week 2 | YAML-based DLP policy management. |
| `bairav skill package/deploy/run/list/logs` | Week 3 | OCI packaging + K8s Job runtime. |

## Module conventions

- Cobra is the CLI framework; no custom flag parsing.
- Each subcommand lives in its own file under `cmd/`.
- User-facing errors go to stderr with a `bairav:` prefix (handled centrally in `cmd/root.go`).
- Structured logs (for debugging) go to stdout as JSON — separate from the UX output.

## Why Cobra

Cobra is the de-facto Go CLI library, used by `kubectl`, `helm`, `hugo`, and most CNCF tooling. Zero custom abstractions; new contributors recognize it instantly.
