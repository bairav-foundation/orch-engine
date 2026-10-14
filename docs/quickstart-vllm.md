# Quickstart: serve on a GPU with vLLM (experimental)

The default backend, llama-server, runs a GGUF on CPU, which is why a model deploys on any laptop or cluster. On a GPU node, [vLLM](https://docs.vllm.ai) serves many more requests at once. A Model can ask for it with `--backend vllm`:

```bash
bairav model deploy Qwen/Qwen2.5-1.5B-Instruct --target k8s --name qwen-gpu --backend vllm
```

Nothing else changes: the Model keeps its Service, port and status endpoint, Agents reach it the same way, and DLP (Agent sidecars or `--dlp-ref` on the Model) works as it does with llama-server.

It's marked experimental because we test it less than llama-server: the unit tests cover what the operator builds, but there's no GPU in our CI. Tell us how it goes.

## What you need

- A Kubernetes node with an NVIDIA GPU and the [NVIDIA device plugin](https://github.com/NVIDIA/k8s-device-plugin), so Pods can request `nvidia.com/gpu`.
- A **safetensors** repo, not a GGUF one: `Qwen/Qwen2.5-1.5B-Instruct`, not `Qwen/Qwen2.5-1.5B-Instruct-GGUF`. `--file` doesn't apply.
- For gated models (Llama, Gemma), a Hugging Face token: `kubectl create secret generic hf-token --from-literal=token=hf_...` and `spec.hfTokenSecretRef` on the Model.

## What the operator runs

| | llama-server (default) | vLLM |
|--|--|--|
| Image | `ghcr.io/bairav-foundation/serving` | `vllm/vllm-openai:v0.20.2` |
| Weights | one GGUF file | the whole repo (safetensors) |
| Hardware | CPU | 1 `nvidia.com/gpu` by default (`--gpus N` for more) |
| Defaults | 500m CPU, 2–4Gi | 2 CPU, 8–32Gi memory, 2Gi `/dev/shm` |
| `--ctx-size` | `--ctx-size` | `--max-model-len` |

The vLLM server answers to three model names: the repo id, the Model's name and `default`. Clients that send any of them work, which matters because vLLM, unlike llama-server, rejects a name it doesn't serve.

The image is pinned so a vLLM release can't change a running cluster. To use another version, set `spec.image` on the Model or `BAIRAV_VLLM_IMAGE` on the operator Deployment. Check the [vLLM Docker docs](https://docs.vllm.ai/en/latest/deployment/docker/) for current tags.

## Upgrades and DLP

- `bairav model upgrade qwen-gpu --to Qwen/Qwen2.5-7B-Instruct --target k8s` swaps the repo and keeps the vLLM backend. With the default surge strategy the new Pod needs its own GPU while the old one is still serving; on a single-GPU node use `--strategy recreate`.
- A later `bairav model deploy` without `--backend` keeps vLLM; pass `--backend llama-server` to switch back.
- With `--dlp-ref`, the DLP container takes the Service port and vLLM listens on `127.0.0.1:18080` only, as with llama-server.

## Checking it

```bash
kubectl -n default get model qwen-gpu -w            # wait for Ready (the first download takes a while)
kubectl -n default logs deploy/qwen-gpu -c serving   # vLLM's own start-up log
kubectl -n default port-forward svc/qwen-gpu 8080:8080 &
bairav eval run examples/evals/basics.yaml --endpoint http://localhost:8080/v1
```

Weights are cached in an `emptyDir`, so a Pod restart downloads them again. A persistent cache is on the list.

## Limits

- Kubernetes only. On Docker, run vLLM yourself and connect it with `bairav model deploy <model> --provider openai-compatible --base-url http://host:8000/v1`, which puts the DLP gateway in front of it.
- One Pod per Model; no tensor parallelism across nodes.
- NVIDIA GPUs only for now (the default image is the CUDA build).
