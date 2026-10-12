# `cli/`: the `bairav` CLI

Everything a user does with Orch Engine goes through `bairav <resource> <action>`.

## Build and run

```bash
# from the repo root
make build-cli
./bin/bairav --help

# or from this module
go run ./ version
go test ./...
```

## Commands

| Command | What it does |
|---------|--------------|
| `bairav model` | pull, list, serve, delete, deploy, undeploy, upgrade, rollback, status, discover |
| `bairav skill` | deploy and manage MCP servers |
| `bairav agent` | deploy agents wired to models and skills |
| `bairav dlp` | run the DLP proxy, test a policy, list packs |
| `bairav grant` | let another namespace use your model or skill |
| `bairav chat` | chat with a deployed model |
| `bairav ps` | list what's running |
| `bairav dashboard` | local read-only dashboard |
| `bairav visualize` | print the graph |
| `bairav version` | build version |

Full guides: [`docs/`](../docs/).

## Conventions

- Cobra for commands and flags. One file per command under `cmd/`.
- User-facing errors go to stderr with a `bairav:` prefix (handled in `cmd/root.go`).
- Library code lives under `internal/` and doesn't print.
