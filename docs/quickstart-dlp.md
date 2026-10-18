# Quickstart — Prompt DLP

One flag puts a DLP proxy in front of a model, so structured personal data and secrets in prompts are redacted or blocked before the model sees them.

The BAIRAVS DLP proxy is an HTTP server that sits between any OpenAI-compatible client and any OpenAI-compatible model endpoint. It inspects every `/v1/chat/completions`, `/v1/completions`, and `/v1/embeddings` request for PII and secrets (email, SSN, credit card with Luhn, US phone, AWS access keys) and:

- **redacts** each hit to `[REDACTED:LABEL]` before forwarding (default), **or**
- **blocks** the request with HTTP 403 (`--action block`), **or**
- **audit-only** logs every hit but forwards unchanged (`--action audit_only`) — useful for measuring exposure before turning enforcement on.

Every decision is written to `~/.bairav/dlp/audit-YYYY-MM-DD.jsonl` (one JSON line per request).

---

## 60-second demo

Three terminals. Total wall time after the model boots: under a minute.

**Terminal 1 — bring up a model (default docker target).**

```bash
bairav model deploy Qwen/Qwen2.5-0.5B-Instruct-GGUF
# wait for it to be ready (the entrypoint pulls the GGUF on first boot)
bairav chat --endpoint http://127.0.0.1:8080/v1 --wait 5m
# Ctrl-D to leave once you've confirmed it's responsive
```

**Terminal 2 — start the DLP proxy in front of it.**

```bash
bairav dlp serve --upstream http://127.0.0.1:8080/v1   # listens on 127.0.0.1:8090
```

Output:
```
bairav dlp serve
  listening: http://127.0.0.1:8090
  upstream:  http://127.0.0.1:8080/v1
  action:    redact
  audit:     /home/you/.bairav/dlp/audit-2026-06-08.jsonl
Detectors: EMAIL, SSN, CREDIT_CARD, PHONE_US, AWS_ACCESS_KEY

Point bairav chat at the proxy:
  bairav chat --endpoint http://127.0.0.1:8090/v1
```

**Terminal 3 — chat through the proxy.**

```bash
bairav chat --endpoint http://127.0.0.1:8090/v1
```

Now type a prompt with deliberately sensitive content:

```
you> Please summarise: alice@example.com signed up with SSN 123-45-6789 and card 4242 4242 4242 4242.
bot> [summary that talks about a "[REDACTED:EMAIL]" with "[REDACTED:SSN]" and "[REDACTED:CREDIT_CARD]"]
```

The model never saw the real email, SSN, or card — only the placeholders. Check the audit log:

```bash
tail -n 1 ~/.bairav/dlp/audit-2026-06-08.jsonl | jq
```

```json
{
  "timestamp": "2026-06-08T22:15:03.124Z",
  "method": "POST",
  "path": "/v1/chat/completions",
  "action": "redact",
  "labels": ["EMAIL", "CREDIT_CARD", "SSN"],
  "hit_count": 3,
  "upstream_code": 200,
  "latency_ms": 412,
  "matches": ["al…om (17)", "12…89 (11)", "42…42 (19)"]
}
```

`matches` is masked by default: first and last two characters plus the length. That's enough to spot a false positive without copying the PII into every log pipeline. `--audit-matches raw` (or `BAIRAV_DLP_AUDIT_MATCHES=raw`) logs the verbatim text when your SOC needs it. `none` drops it. The audit file is created `0600`.

That's the whole feature in a nutshell.

---

## Trying detectors without a model

The fastest tuning loop is `bairav dlp test`. No proxy, no model, just one-shot detector output:

```bash
bairav dlp test "ping alice@example.com or call (415) 555-1212"
```

```
Hits: 2
  LABEL     START  END  MATCH
  EMAIL     5      22   alice@example.com
  PHONE_US  31     45   (415) 555-1212

Redacted:
ping [REDACTED:EMAIL] or call [REDACTED:PHONE_US]
```

You can also pipe stdin:

```bash
cat suspicious-prompt.txt | bairav dlp test
```

---

## Modes

```bash
# Default — replace hits with [REDACTED:LABEL] before forwarding.
bairav dlp serve --upstream … --action redact

# Reject the request with HTTP 403; nothing reaches the model.
bairav dlp serve --upstream … --action block

# Log every hit, forward the prompt unchanged. Use this in shadow mode
# before turning enforcement on — measure exposure first.
bairav dlp serve --upstream … --action audit_only
```

---

## What's detected

| Label | What | Notes |
|-------|------|-------|
| `EMAIL` | Email addresses | Loose RFC; minimum 2-letter TLD |
| `SSN` | US Social Security Number | `NNN-NN-NNNN`, `NNN NN NNNN`, `NNN.NN.NNNN`; a bare 9-digit run only right after "SSN" / "social security". Never-issued numbers (000, 666, 9xx area) are ignored. |
| `CREDIT_CARD` | Card numbers (ISO 7812 PAN) | 13–19 digits with space, dash or dot separators, Luhn-validated. Still found with a CVV or expiry stuck to it (`4111111111111111 123`). |
| `PHONE_US` | US phone numbers | `(415) 555-1212`, `415-555-1212`, etc. Has no context, so a 3-3-4 order number also matches. |
| `AWS_ACCESS_KEY` | AWS Access Key IDs | `AKIA…`, `ASIA…`, other AWS prefixes |
| Secrets | `OPENAI_KEY`, `ANTHROPIC_KEY`, `GITHUB_TOKEN`, `GITLAB_TOKEN`, `SLACK_TOKEN`, `STRIPE_KEY`, `GOOGLE_API_KEY`, `HF_TOKEN`, `AZURE_STORAGE_KEY`, `AWS_SECRET_KEY`, `JWT`, `PRIVATE_KEY` | Vendor-prefix / structure anchored (gitleaks-style), so precise enough to be on by default. Generic "high-entropy string" detection is deliberately left out. |

**Evasion resistance.** Text is normalised before scanning:
- full-width digits and letters are folded to ASCII
- Indic and Arabic digits are folded to ASCII (Devanagari, Tamil, Bengali, Telugu, Kannada, Malayalam, Gujarati, Gurmukhi, Oriya, Arabic-Indic)
- zero-width characters and soft hyphens are dropped

Hits are mapped back to the original text, which is what gets redacted.

**What the proxy inspects.** Every string in the request JSON on an inference route: messages, system prompts, content parts, tool definitions and arguments, tool results, metadata, Anthropic documents, Responses API prompt variables, and so on. The only strings skipped are a short reviewed list of identifier fields (`model`, `role`, `type`, `id`, tool-call ids, `media_type`, `signature`, finish/stop reasons) and inline binary payloads (base64 images/PDFs, `data:` URIs). Those binaries are **not** inspected.

**Fail closed.** The proxy refuses three kinds of requests:
- a request body that isn't a JSON object on an inference route (400)
- any non-GET request to a route it doesn't inspect (404). That includes llama-server's native `/completion`, `/infill` and `/tokenize`, which would otherwise be an uninspected side door.
- path spelling variants (`/v1/Chat/Completions/`, `//`) are canonicalised before matching, and upstream redirects are returned to the caller, never followed with the body.

Not detected yet:
- names and addresses (needs an NLP backend)
- emails written as "john dot doe at acme dot com"
- international phone numbers outside the regional packs
- content inside images/PDFs
- prompt injection

---

## Enforcement mode — DLP that can't be bypassed

Everything above runs DLP as an *opt-in* proxy: it works only if the developer points their client at the proxy port. Anyone who knows the raw model port can skip DLP entirely by hitting the model directly. Fine for "protect me from myself" scenarios, not fine for a governance story.

**`--dlp` on `bairav model deploy`** flips the deployment into enforcement mode. The model container is bound to the compose internal network only (no host port publish), and the DLP proxy runs as a sidecar service that is the *only* thing exposed on the host. There is no bypass — the raw model port doesn't exist from the host's perspective.

With a release, the DLP image is pulled from `ghcr.io/bairav-foundation/dlp` automatically. From source, build it once with `make image-serving image-dlp`.

**Deploy with enforcement:**

```bash
bairav model deploy Qwen/Qwen2.5-0.5B-Instruct-GGUF --dlp
```

Output (excerpt):

```
Target:    docker (compose)
Name:      qwen2-5-0-5b
Repo:      Qwen/Qwen2.5-0.5B-Instruct-GGUF
DLP:       enforce (proxy is the only entry point)
  action:  redact (per-rule action overrides in policy still win)
Endpoint:  http://127.0.0.1:8080/v1
StackDir:  /home/you/.bairav/compose/qwen2-5-0-5b
…
✓ Stack up. …
  Note:    the raw model port is bound to the compose network only.
           Prompts reach the model only through the DLP proxy.
```

**Chat exactly the same way as the unenforced path** — same URL, same client:

```bash
bairav chat --endpoint http://127.0.0.1:8080/v1 --wait 5m
```

The URL you point at is still `http://127.0.0.1:8080/v1`, but under the hood 8080 is now the DLP proxy container, not the model. Every request is inspected before it reaches the model. Try to short-circuit the proxy — you can't, because the raw model port doesn't exist on the host.

**Watch the audit stream:**

```bash
docker logs -f bairav-<name>-dlp | jq 'select(.action)'
```

In enforce mode the DLP container logs its audit records to stdout (rather than a file inside the container — the distroless nonroot user can't write to arbitrary paths). Every prompt shows up as a JSON line with `labels`, `label_actions`, `hit_count`, and the `matches`. Pipe through `jq` to explore; a proper log-shipper like Loki/Splunk/OpenSearch consumes it identically because it's plain JSONL on stdout.

**Layer in a custom policy:**

```bash
curl -fsSLO https://raw.githubusercontent.com/bairav-foundation/orch-engine/main/examples/dlp-policy.yaml
bairav model deploy Qwen/Qwen2.5-0.5B-Instruct-GGUF \
  --dlp \
  --dlp-action block \
  --dlp-policy dlp-policy.yaml
```

The policy file is bind-mounted read-only into the DLP container at `/etc/bairav/dlp-policy.yaml`; the proxy loads it on startup exactly as if you'd passed `--policy` to `dlp serve` on the host.

**Under the hood.** `--dlp` sets `Spec.DLPMode = compose.DLPModeEnforce` in the CLI. Renderer emits a two-service docker-compose.yaml: one `serving` service (unpublished, `expose:` only) and one `dlp` service (publishes the host port, forwards to `http://serving:8080/v1`). The compose default bridge network is the enforcement mechanism. Kept as a string enum (not a bool) so we can add intermediate modes (`on` = proxy alongside but not required) later without a schema break.

---

## Built-in packs — zero-YAML PII coverage for multinationals

Every rule in the CLI lives in one of three tiers:

| Tier | What's in it | When it runs |
|------|--------------|--------------|
| **Built-ins** | Universal shapes (email, credit card, JWT, AWS keys) + US-centric (SSN, US phone) | Always on |
| **`global-safe` pack** | Checksum-anchored national IDs: Aadhaar (Verhoeff), UK NHS (Mod-11), with context rules | **On by default** |
| **Regional packs** (`india`, `uk`, ...) | Shape-only rules that can false-positive outside their region | **Opt-in** via `--pack` |

**Why `global-safe` is on by default, and how it stays precise.** A single check digit only filters about 90% of random numbers. Shape plus checksum alone flagged about 10% of random 12-digit order numbers as AADHAAR, and about 9% of 3-3-4 phone numbers as UK_NHS. So the pack adds context:
- **Aadhaar** (first digit 2-9) counts on its own in the written 4-4-4 layout (`2345 6789 0124`). A bare 12-digit run needs "aadhaar" / "aadhar" / "uidai" / "आधार" / "ஆதார்" within 48 characters.
- **NHS** always needs "nhs" or "patient number" nearby, because 3-3-4 is also the US phone layout.

Measured on 20,000 random samples each (`internal/dlp/packs/fp_test.go`, run in CI): under 1% for both. Real IDs written the usual way are still caught. Shape-only rules (like `INDIA_MOBILE`, "10 digits starting 6-9") stay opt-in per region.

**See what's shipped:**

```bash
bairav dlp packs list
```

Output:
```
PACK         RULES                                        DEFAULT
global-safe  AADHAAR, UK_NHS                              ✓
india        GSTIN, INDIA_LANDLINE, INDIA_MOBILE, PAN
uk           UK_NINO
```

**Enable regional packs on top:**

```bash
# Default (global-safe on, no regionals):
bairav dlp serve --upstream http://127.0.0.1:8080/v1

# Add India + UK regional shape-only rules:
bairav dlp serve --upstream http://127.0.0.1:8080/v1 --pack india,uk
```

**Disable the defaults** (rarely needed — probably for a highly-tuned custom-rules-only deployment):

```bash
bairav dlp serve --upstream http://127.0.0.1:8080/v1 --pack none --policy custom.yaml
```

**Stack your own custom rules** on top of packs:

```bash
bairav dlp serve --upstream http://127.0.0.1:8080/v1 \
  --pack india,uk \
  --policy corp-customer-ids.yaml
```

Rules from every source (built-ins + packs + `--policy` files) stack. Cross-source label collisions fail at load time with a clear message so ambiguities surface immediately.

**Getting test data for a rule you're authoring:**

```bash
bairav dlp checksum verhoeff 12345678901    # returns valid Aadhaar-shape
bairav dlp checksum mod11    401023213      # returns valid NHS-shape
bairav dlp checksum luhn     4111111111111  # returns valid credit-card
```

Append the printed check digit to the prefix and you have a valid test vector for that rule's regex + checksum combination.

---

## Extending — write your own YAML

The built-ins and packs are just YAML files with the same schema — packs happen to be embedded in the binary via `//go:embed`. To add rules the CLI doesn't ship (corporate customer IDs, internal token formats, jurisdictions we haven't authored yet), write a YAML file and pass it via `--policy`:

```yaml
version: v1
rules:
  - label: CUST_ID
    pattern: 'CUST-\d{7}'
    action: redact
  - label: MY_INTERNAL_ID
    pattern: '\b\d{9,12}\b'
    checksum: luhn       # only accept Luhn-valid IDs; drop random digit runs
    action: redact
```

Load alongside the built-ins and packs:

```bash
bairav dlp serve --upstream http://127.0.0.1:8080/v1 --policy corp-rules.yaml
```

**Supported checksums (as of today):**

| Value | Algorithm | Common uses |
|-------|-----------|-------------|
| `luhn` | Luhn / Mod-10 | Credit cards, IMEI, some national IDs |
| `mod10` | Alias for `luhn` | Same algorithm; use whichever name matches your team |
| `verhoeff` | Verhoeff (dihedral D_5) | India Aadhaar, some Sri Lankan/Zimbabwean IDs |
| `mod11` | Mod-11 (UK NHS variant) | UK NHS, some Nordic/LatAm IDs |

**Why we ship packs as YAML, not compiled detectors.** A pack is authored, reviewed, and tuned by someone who knows the jurisdiction. Baking every jurisdiction's rules into the binary means shipping US assumptions about "what PII looks like" to a Brazilian bank or a Japanese hospital. The pack model — YAML + `//go:embed` for defaults, `--policy` for user extensions — keeps every geography's detection versionable independently of the binary and lets non-Go authors contribute rules.

---

## Notes

- **Streaming responses** are passed through unchanged. The proxy redacts the prompt going *into* the model; the model's reply streams back token-by-token without buffering.
- **Audit log retention** is up to you. The file is plain JSONL — point Loki, Splunk, OpenSearch, or `tail -f | jq` at it.
- **Audit matches** are masked by default (see above). Switch to `raw` only if your audit pipeline is cleared to hold PII.
- **Limits**: request bodies over 32 MiB get `413` (never buffered). Buffered replies over 64 MiB get `502`. The server has a 10 s header-read timeout and a 120 s idle timeout, and no write timeout, so long streams keep working.
- **Performance**: detectors are all compiled regex; expect single-digit microseconds for typical prompts. Proxy adds <1 ms of overhead at p99 on a laptop.
