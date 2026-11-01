# How DLP works

What the Orch Engine DLP engine detects, how it scans traffic, and where its limits are. The code is in `cli/internal/dlp`.

## Where it sits

The DLP engine is the `bairav dlp serve` proxy. It can run in six places:

- As a **sidecar** next to an Agent (Kubernetes, `dlpRef` on the Agent). The agent's model endpoint points at the sidecar on localhost.
- **Inside the Model Pod** (Kubernetes, `dlpRef` on the Model). The proxy takes the Service port and the model listens on 127.0.0.1 only, so every caller goes through DLP.
- **In front of an MCP server** (`dlpRef` on a Skill, or `bairav skill deploy --dlp` on Docker). Tool-call arguments and tool results are scanned; a NetworkPolicy leaves the proxy as the only way into the Skill Pod. For a remote MCP server (a Skill with a URL) the same proxy runs as a gateway that also holds the server's token. See [Skills](quickstart-skill.md#dlp-on-tool-calls).
- In front of a model in **enforce mode** (`bairav model deploy --dlp` on Docker). The model is only reachable through the proxy.
- As a **LiteLLM guardrail** (`bairav dlp serve --litellm-guardrail`). LiteLLM sends prompts and answers to it and applies the verdict. See [DLP as a LiteLLM guardrail](quickstart-litellm.md).
- As a **gateway** for cloud models (`--provider openai|anthropic|openai-compatible`). The gateway holds the provider key, strips any key the caller sends, pins the model, and only forwards inference paths.

## What it scans

Every string in the request JSON is scanned, not only the `messages` field. A short, reviewed list of identifier keys (model name, IDs, roles) is skipped. Scanning only known fields left new fields such as tool descriptions and document blocks as silent bypasses, so the default is to scan everything.

Responses are scanned too, when the policy direction is `response` or `both`. Streaming responses (server-sent events, OpenAI and Anthropic formats) are redacted as they stream: the proxy holds back the last 128 bytes of each stream so a value split across two chunks is still caught. The final token arrives at the same time; text appears about 128 bytes later than it would without DLP.

Requests the proxy can't parse are rejected with `400`, not passed through. Non-GET requests to paths it doesn't inspect get `404`. Bodies over 16 MiB are refused.

## Normalisation

Before detection, text is folded to an ASCII copy so simple tricks don't slip past the patterns:

- full-width digits (`４１１１…`)
- Indian and Arabic digit scripts (Devanagari, Bengali, Tamil, Arabic-Indic and others)
- zero-width characters and soft hyphens inserted between digits

A hit in the folded copy is mapped back to the exact span in the original text, and that span is what gets redacted.

## Detectors

Built in, always on unless you turn them off:

| Label | What | Validation |
|-------|------|------------|
| `EMAIL` | email addresses | pattern |
| `CREDIT_CARD` | card numbers, with spaces, dashes or dots | Luhn checksum |
| `SSN` | US Social Security numbers | format rules (no 000, 666, 9xx area) |
| `PHONE_US` | US/Canada phone numbers | pattern |
| `AWS_ACCESS_KEY` | AWS access key IDs | prefix |
| secrets | Anthropic, OpenAI, GitHub, GitLab, Slack, Stripe, Google, Hugging Face, Azure storage and AWS secret keys, JWTs, PEM private keys | vendor prefix or fixed structure |

Packs (`--pack`, or `packs:` in a DLPPolicy):

| Pack | Labels |
|------|--------|
| `global-safe` (default) | `AADHAAR` (Verhoeff checksum), `UK_NHS` (Mod-11 checksum) |
| `india` | `AADHAAR`, `PAN`, `GSTIN`, `INDIA_MOBILE`, `INDIA_LANDLINE` |
| `uk` | `UK_NHS`, `UK_NINO` |

You can add your own rules in YAML: a label, a pattern, an optional checksum and an action. See [Prompt DLP](quickstart-dlp.md).

### False positives

A single check digit lets about 1 in 10 random numbers of the right length through. So Aadhaar and NHS numbers need a keyword nearby ("aadhaar", "nhs", "patient number" and so on), unless an Aadhaar number is written in its usual `1234 5678 9012` grouping. With those rules, the pack tests measure 0 false positives on random 12-digit order numbers and 10-digit phone numbers. The tests are in `cli/internal/dlp/packs/fp_test.go`.

## Actions

Each rule has an action:

- **redact**: replace the value with `[REDACTED:LABEL]` and forward.
- **block**: refuse the request with `403`.
- **audit_only**: forward unchanged, log the hit.

If one request matches several rules, the strongest action wins (block, then redact, then audit).

## Images, PDFs and audio

The detectors read text. A screenshot, a scanned ID card or a PDF statement carries the same data as pixels, which they can't read yet. Reading them (OCR) is coming in v0.2.4 for images and v0.2.5 for PDFs; until then the proxy never lets an attachment through silently. Every request that carries an image, PDF, audio clip or file is recorded with `"uninspected": ["image"]` in the audit log, and the policy decides whether it may pass:

| `attachments` | What happens |
|---|---|
| `audit` (default) | Forwarded. Recorded as uninspected, and the response carries `X-Bairav-DLP-Uninspected: image` |
| `block` | Refused with `403` (`dlp_attachment_blocked`); nothing reaches the model |

Set it on a DLPPolicy (`spec.attachments: block`), on a cloud Model (`spec.cloud.dlpAttachments: block`), or with `bairav dlp serve --attachments block`. If your team pastes screenshots of customer data, use `block` until OCR ships.

It recognises images, files and audio in model requests in OpenAI (Chat Completions and Responses) and Anthropic shapes, image links (`https://...`) as well as inline data, and any base64 `data:` URI anywhere in the body. In LiteLLM guardrail mode it applies to the `images` LiteLLM passes.

## Audit log

Every request produces one JSON line: time, agent, model, direction, action, labels found, and latency. Matched values are masked by default (`--audit-matches masked`); `raw` and `none` are also available. If the audit log can't be written, the proxy reports it, and with `--audit-required` it refuses traffic instead.

## Latency

Detection is a set of compiled regular expressions run over the text, with a cheap literal pre-check before the costlier secret patterns. We haven't published benchmarks yet. The audit log records the latency of every request, so you can measure it on your own traffic.

## Limits

Be clear about what this does not do:

- It finds **structured** data: IDs, card numbers, keys, emails. It doesn't use a language model or named-entity recognition, so free-text names, addresses and medical details aren't detected.
- It doesn't read images, PDFs, audio or files yet. They're recorded as uninspected, or refused with `attachments: block` (see [Images, PDFs and audio](#images-pdfs-and-audio)).
- For Skills, it sees what goes into and out of the MCP server, not the server's own calls to other APIs.
- With only an Agent sidecar, an agent that ignores its configured endpoint can call the model's Service directly. Put `dlpRef` on the Model to close that path.

Found a way past it? That's a security issue: see the [security policy](../SECURITY.md).
