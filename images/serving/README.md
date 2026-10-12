# `images/serving/` — `bairav/serving:laptop` container image

Minimal LLM-serving container used by the BAIRAVS operator's reconciled `Deployment`. Wraps `llama-server` from llama.cpp with a thin entrypoint that pulls the GGUF file from Hugging Face on first boot.

## Build

```bash
docker build -t bairav/serving:laptop -f images/serving/Dockerfile images/serving/
```

Multi-stage:
1. **builder** — Debian slim + build-essential + cmake; clones `llama.cpp` at the pinned `LLAMA_CPP_TAG` and compiles `llama-server`.
2. **runtime** — Debian slim + curl + jq + the single binary + the entrypoint script. Runs as non-root user `bairav` (uid 10001).

## Footprint goal

Keep the runtime image **under 200 MB**. Track over time:

```bash
docker images bairav/serving:laptop
```

## Load into a local k3d cluster (no registry needed)

```bash
k3d image import bairav/serving:laptop -c bairavs
```

After import, the image is referenceable from inside the cluster as `bairav/serving:laptop`.

## Entrypoint contract

The container reads its config from environment variables — no flags. The K8s operator sets these from the `Model` CR's spec:

| Env var | Required | Default | What it does |
|---------|----------|---------|--------------|
| `BAIRAV_REPO` | yes | — | HF repo slug, e.g. `Qwen/Qwen2.5-0.5B-Instruct-GGUF` |
| `BAIRAV_FILE` | no | smallest `.gguf` in repo | Specific file path inside the repo |
| `BAIRAV_PORT` | no | `8080` | TCP bind port |
| `BAIRAV_HOST` | no | `0.0.0.0` | Bind address (use `0.0.0.0` inside containers) |
| `BAIRAV_CTX_SIZE` | no | backend default | Context window in tokens |
| `BAIRAV_MODELS_DIR` | no | `/models` | Cache dir; mount a PVC here for persistence |
| `HF_TOKEN` | no | — | Auth for gated/private repos |

## Smoke test outside K8s

```bash
docker run --rm -p 8080:8080 \
    -e BAIRAV_REPO=Qwen/Qwen2.5-0.5B-Instruct-GGUF \
    -v "$(pwd)/cache:/models" \
    bairav/serving:laptop

# In another terminal:
curl http://localhost:8080/v1/chat/completions \
    -H 'Content-Type: application/json' \
    -d '{"model":"qwen","messages":[{"role":"user","content":"hi"}]}'
```

## Why we build our own image

- **Footprint visibility from day one** — community images bundle everything; we want to know exactly what ships.
- **Pinned `llama.cpp` revision** — reproducible builds, no surprise upgrades.
- **Custom entrypoint** — pulls from HF at boot using the same conventions as the CLI cache (`/<repo>/<file>`).
- **Non-root by default** — friendlier for hardened K8s setups.

The Backend abstraction in `cli/internal/serve` means we can later add `bairav/serving:gpu-vllm` (vLLM-based) for GPU clusters without touching the operator's reconcile logic — only the image reference and resource requests change.
