# Quickstart — cloud models behind DLP (`--provider`)

Not every team self-hosts. With `--provider`, an OpenAI, Anthropic or any OpenAI-compatible model becomes a bairav Model like any other. It has the same name, endpoint, Agents wiring, `model status`, `upgrade` and `rollback`. Every call goes through the bairav DLP gateway.

```
bairav model deploy gpt-4.1-mini      --provider openai     --name support-model
bairav model deploy claude-sonnet-4-5 --provider anthropic  --name support-model
bairav model deploy my-llama          --provider openai-compatible --base-url http://vllm.gpu:8000/v1
```

The first argument is the provider's model id. Use whatever id your provider account offers; the ones above are examples.

## What the gateway does

Instead of a model server, the Model runs one small container: `bairav dlp serve` in gateway mode (about 50 MB, starts in seconds, no download).

- **Holds the API key.** Agents never see it. The gateway strips any key a caller sends, so it is the only door to the provider.
- **DLP on every request and response.** It scans prompts, system prompts, content parts, tool-call arguments and tool results. Responses are redacted too, including streams. Every decision goes to the audit log (`docker logs` / `kubectl logs`).
- **Pins the model.** Callers can't switch to a pricier model, and swapping models is a server-side change.
- **Allows inference paths only.** `/chat/completions`, `/completions`, `/embeddings`, `/responses`, `/messages`. Anything else (files, fine-tuning, batches, admin) gets a 404. `GET /v1/models` lists only the pinned model.
- **Fails closed.** A body it can't parse is refused, not forwarded.
- **Caller tokens (optional).** Set `BAIRAV_DLP_CLIENT_TOKENS` (or `BAIRAV_DLP_CLIENT_TOKENS_FILE`) and callers must send `Authorization: Bearer <token>` or `X-Api-Key: <token>`; anything else gets 401. The operator doesn't mint per-Agent tokens yet (planned, see the roadmap). Until then, on Kubernetes, anything that can reach the gateway Service can use the provider key, so restrict who can reach it.
- **Loopback by default.** On docker the gateway port is published on `127.0.0.1` only. `BAIRAV_PUBLISH_ADDR=0.0.0.0` opts in to LAN access.

## Docker

```bash
make image-dlp                                 # gateway image (one-time / after upgrades)
export OPENAI_API_KEY=sk-...
bin/bairav model deploy gpt-4.1-mini --provider openai --name support-model
bin/bairav chat --endpoint http://127.0.0.1:8080/v1
docker logs -f bairav-support-model            # live DLP audit
```

The key is written to `~/.bairav/compose/support-model/provider-openai.env` (mode 0600) and loaded with `env_file`. It is never written into `docker-compose.yaml`.

Key lookup order:

1. `--api-key-env VAR`
2. `OPENAI_API_KEY` / `ANTHROPIC_API_KEY`
3. `BAIRAV_PROVIDER_API_KEY`

## Kubernetes

```bash
export ANTHROPIC_API_KEY=sk-ant-...
bin/bairav model deploy claude-sonnet-4-5 --provider anthropic --name support-model --target k8s --namespace team-a
```

The CLI stores the key in the Secret `support-model-anthropic-key`, and only the gateway pod mounts it. To use an existing Secret, pass `--api-key-secret name[:key]`. Or write the Model yourself:

```yaml
apiVersion: orch.bairavs.com/v1alpha1
kind: Model
metadata: { name: support-model, namespace: team-a }
spec:
  provider: anthropic                # local (default) | openai | anthropic | openai-compatible
  repo: claude-sonnet-4-5            # provider model id
  cloud:
    apiKeySecretRef: { name: anthropic-key, key: api-key }
    dlpAction: redact                # redact | block | audit_only
    dlpPacks: [global-safe, india]
    # baseURL: https://...           # required for openai-compatible
```

A cloud Model without a key Secret (or openai-compatible without `baseURL`) goes to `Failed` with a message that says what to add. `kubectl get models` shows a PROVIDER column.

**Key rotation:** after updating the Secret, run `kubectl rollout restart deploy/support-model -n team-a`.

## Swap cloud and local (the point of all this)

Start on GPT. When the self-hosted model is good enough, move with one command. Agent code and endpoints don't change:

```bash
bin/bairav model upgrade support-model --to Qwen/Qwen2.5-1.5B-Instruct-GGUF --provider local --target k8s --namespace team-a
bin/bairav model rollback support-model --target k8s --namespace team-a        # back to GPT, provider and key included
```

The same safety rules as any upgrade apply:

- wait until the new model is served
- smoke prompt
- automatic rollback on failure

On docker, cloud → local keeps DLP on: the local model is deployed behind the DLP proxy, because the gateway was doing DLP before.

## Anthropic: two ways in

- **Native `/v1/messages`** (Anthropic SDK pointed at the gateway). This is the recommended path for production Claude agents. The gateway inspects `system`, `messages` and tool results, redacts responses, and handles streaming events.
- **OpenAI format** (`/v1/chat/completions`). This is forwarded to Anthropic's OpenAI-compatible endpoint, so OpenAI-SDK agents work unchanged. Anthropic describes that layer as meant mainly for testing and comparing models, not long-term production, and some fields are ignored (e.g. `response_format`, `strict`). Use it to try Claude; move to `/v1/messages` for production.

## Not yet

- Streaming tool-argument deltas (`input_json_delta`) and `/responses` replies are passed through without response-side redaction. Request-side inspection covers both.
- Azure OpenAI's `api-key` header style. For now use an Azure endpoint that accepts `Authorization: Bearer`, via `openai-compatible`.
- Remote MCP Skills (a URL instead of a pod). Next on the list.
