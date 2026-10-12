# Coding standards

Rules the codebase follows. If you're adding something that doesn't fit one of these rules, the rule is wrong — update it.

## Go

### Formatting and linting

- `gofmt -s` is mandatory. CI fails on any diff.
- `goimports` for import grouping: stdlib, blank line, third-party, blank line, local (`github.com/bairav-foundation/...`).
- `golangci-lint` with the config at repo root is the source of truth. Install via `make tools`.
- No `//nolint:...` suppressions without a one-line reason comment next to them.

### Naming

- Package names: short, lowercase, no underscores. `llamaserver`, not `llama_server` or `llamaServerPkg`.
- Exported identifiers: document them. Every exported type, function, and constant gets a doc comment starting with the identifier name.
- Error variables: `ErrSomething` for sentinel errors. Wrap with `fmt.Errorf("context: %w", err)`.
- Interfaces: small. If an interface has more than 3 methods, question whether it should be split.

### Errors

- Always wrap with context: `fmt.Errorf("load policy %q: %w", name, err)`.
- Never `panic()` in library code. Only `main()` functions may panic, and only when the program cannot proceed.
- Return errors, don't log and return nil. The caller decides whether to log.
- `errors.Is` and `errors.As` over string matching.

### Logging

- Structured JSON to stdout using `log/slog`.
- Levels: `Debug`, `Info`, `Warn`, `Error`. No custom levels.
- Log keys in `snake_case`: `slog.Info("deploying model", "model_ref", ref, "namespace", ns)`.
- Never log secrets, tokens, or prompt contents at `Info` level. If you must log a prompt for debugging, use `Debug` and redact.

### Context

- Any function that does I/O takes `ctx context.Context` as the first argument.
- Never store a `context.Context` in a struct field.
- Don't use `context.TODO()` in merged code. `context.Background()` at program entry points only.

### Testing

- Table-driven tests by default. Each case gets a `name` field.
- Use the stdlib `testing` package and `testify/require` only where it materially helps.
- Unit tests colocate with the code under test (`foo_test.go` next to `foo.go`).
- End-to-end smoke tests live in `scripts/smoke-*.sh` and run against a real cluster.

## Python (examples only)

- Python 3.11. Type hints on every function signature.
- `ruff` for lint and format.
- No `print()` in library code — use the `logging` module.

## YAML (CRDs, manifests, policies)

- 2-space indent, no tabs.
- `apiVersion` and `kind` at the top.
- List entries always use `-` with a space; no flow-style lists (`[a, b, c]`) in shipped manifests.

## Shell scripts

- `#!/usr/bin/env bash` + `set -euo pipefail` on line 2.
- Always quote variable expansions: `"$foo"`, not `$foo`.
- Functions over inline blocks when a script exceeds 30 lines.

## Commits and PRs

### Conventional Commits

Every commit message follows [Conventional Commits](https://www.conventionalcommits.org/):

```
<type>(<scope>): <subject>

<body>

<footer>
```

Types: `feat`, `fix`, `docs`, `refactor`, `test`, `chore`, `ci`, `perf`.
Scope is the area: `cli`, `operator`, `dlp`, `images`, `docs`.

Example: `feat(cli): add 'bairav model pull' for HF GGUF downloads`

### PR discipline

- Small PRs. If the diff exceeds ~400 lines excluding tests and vendored files, split it.
- Every PR links to an issue.
- Tests added or updated alongside the change. A PR that claims "no tests needed" must say why in the description.
- CI must be green before merge. No bypassing the lint or test gates.

## File layout per Go module

```
<module>/
├── go.mod
├── main.go              (if it's a binary)
├── cmd/                 (subcommand packages)
├── internal/            (package code that shouldn't be imported externally)
│   ├── foo/
│   └── bar/
├── pkg/                 (only if genuinely reusable outside this module — avoid by default)
└── testdata/            (golden files and fixtures)
```

`internal/` is the default. `pkg/` is opt-in and needs a reason.

## When in doubt

1. Read the existing code in the module you're editing — match its style.
2. If the existing style violates a rule in this doc, fix the existing code in a separate PR.
3. Ask in the PR if the rule itself seems wrong for the case you're hitting.
