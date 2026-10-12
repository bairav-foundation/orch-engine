# Quickstart: BAIRAVS DLP as a LiteLLM guardrail

Already routing your model traffic through [LiteLLM](https://docs.litellm.ai/)? You can add BAIRAVS DLP to it without moving anything: LiteLLM calls the DLP engine as a guardrail, through its [Generic Guardrail API](https://docs.litellm.ai/docs/adding_provider/generic_guardrail_api), and applies the verdict.

You get the same detectors as everywhere else in Orch Engine: Aadhaar, PAN and the other India and UK identifiers from the packs, card numbers with Luhn, SSNs, emails, and API keys for a dozen vendors, with the same text folding and the same audit log. See [How DLP works](how-dlp-works.md).

## 1. Run the guardrail

```bash
bairav dlp serve --litellm-guardrail --listen 127.0.0.1:8091 --pack global-safe,india
```

It serves one endpoint, `POST /beta/litellm_basic_guardrail_api`, plus `/healthz`. Nothing is forwarded anywhere: LiteLLM sends the texts, the guardrail sends back a verdict.

In a container, the same image works with environment variables:

```bash
docker run --rm -p 127.0.0.1:8091:8090 \
  -e BAIRAV_DLP_MODE=litellm-guardrail \
  -e BAIRAV_DLP_PACK=global-safe,india \
  -e BAIRAV_DLP_AUDIT=- \
  ghcr.io/bairav-foundation/dlp:latest
```

To require a token, set `BAIRAV_DLP_CLIENT_TOKENS` (16+ characters) on the guardrail and put the same value in LiteLLM's `api_key`.

## 2. Tell LiteLLM about it

In LiteLLM's `config.yaml`:

```yaml
litellm_settings:
  guardrails:
    - guardrail_name: bairav-dlp-in
      litellm_params:
        guardrail: generic_guardrail_api
        mode: pre_call            # check prompts before they reach the model
        api_base: http://127.0.0.1:8091
        # api_key: os.environ/BAIRAV_DLP_TOKEN
    - guardrail_name: bairav-dlp-out
      litellm_params:
        guardrail: generic_guardrail_api
        mode: post_call           # check answers before they reach the caller
        api_base: http://127.0.0.1:8091
```

LiteLLM's defaults fail closed: if the guardrail is unreachable or errors, the call is blocked. Keep it that way for DLP.

## 3. Try it

```bash
curl -s http://127.0.0.1:4000/v1/chat/completions \
  -H "Authorization: Bearer $LITELLM_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4.1-mini","messages":[{"role":"user","content":"Summarise: customer aadhaar 2345 6789 0124, email jane@corp.com"}]}'
```

The model receives `[REDACTED:AADHAAR]` and `[REDACTED:EMAIL]`. The guardrail's audit log shows the decision.

## What the guardrail decides

| Found | Verdict |
|-------|---------|
| nothing, or only `audit_only` hits | `NONE`: the call proceeds unchanged |
| a `redact` hit in the text | `GUARDRAIL_INTERVENED`: the texts come back with `[REDACTED:LABEL]` in place |
| a `block` hit anywhere | `BLOCKED`, with the labels as the reason |
| a `redact` hit inside tool-call arguments | `BLOCKED`: this API can't rewrite tool calls, and passing them through would leak |

The default action is `redact`; change it with `--action block` or per rule in a policy file (`--policy`).

## Limits

- Images aren't inspected.
- LiteLLM decides which parts of a request it sends as `texts`; fields it doesn't extract aren't seen. When you control the traffic path, the Orch Engine proxy or gateway scans every string in the request instead.
