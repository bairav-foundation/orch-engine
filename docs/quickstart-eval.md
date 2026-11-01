# Quickstart: check a model with `bairav eval`

Before you swap a model, put a DLP policy in front of it or point a team at it, check that it still does what you need. `bairav eval` sends a list of prompts to any OpenAI-compatible endpoint and checks each reply.

## Start with the built-in suites

No YAML needed. Four suites ship inside the CLI:

```bash
bairav eval list
bairav eval run --builtin basics,json,privacy --endpoint http://localhost:8080/v1
```

| Suite | Checks | Passes at |
|-------|--------|-----------|
| `basics` | follows simple instructions, easy facts, small sums, short answers, no "as an AI" filler | 80% |
| `json` | valid JSON when asked, with the right keys and types, and nothing else | 80% |
| `privacy` | sample text without made-up emails, phone, card or ID numbers (`--pack` picks the detectors) | 75% |
| `dlp` | a DLP proxy in front of the model redacts emails and cloud keys. Point `--endpoint` at the proxy | 100% |

The thresholds leave room for small local models to miss a case now and then. Each suite is a plain YAML file: to change one, copy it and edit the copy.

```bash
bairav eval init --from basics > my-basics.yaml
```

## Your own suite

```bash
bairav eval init > my-suite.yaml
bairav eval run my-suite.yaml --endpoint http://localhost:8080/v1
```

```
Suite starter: 4 cases against http://localhost:8080/v1

  ✓ follows-instructions         1/1     212 ms
  ✓ knows-basics                 1/1     305 ms
  ✗ json-output                  0/1     611 ms  reply is not valid JSON
  ✓ no-pii-in-reply              1/1     980 ms

Score 3/4 (75%), needs 100%: FAIL   (2.1s)
bairav: eval starter: http://localhost:8080/v1 scored 3/4, below the 100% threshold
```

It exits 1 when any suite scores below its threshold, so the same command works in CI. Built-in suites and files mix in one run: `bairav eval run --builtin basics my-suite.yaml`.

## Writing a suite

```yaml
name: support-bot
threshold: 0.9            # share of cases that must pass (default 1 = all)
system: You are a support assistant for an Indian bank. Be brief.
defaults:
  temperature: 0          # always sent, so runs repeat (servers default to ~0.8)
  max_tokens: 256
  timeout_seconds: 120
cases:
  - name: refund-policy
    prompt: How many days do I have to dispute a card charge?
    expect:
      contains: [days]
      not_contains: ["as an ai"]
      max_chars: 400

  - name: structured
    prompt: 'Return {"intent": ..., "urgent": true|false} for: "my card was stolen"'
    expect:
      json: true
      regex: ['"urgent"\s*:\s*true']

  - name: flaky-but-ok
    prompt: Suggest a name for a savings account for kids.
    repeat: 5
    min_pass: 4           # 4 of 5 attempts must pass
    expect:
      max_latency_ms: 3000
```

| Check | Passes when |
|-------|-------------|
| `contains: [..]` | every string is in the reply (case-insensitive) |
| `not_contains: [..]` | none of them is |
| `regex: [..]` / `not_regex: [..]` | every pattern matches / none does (Go regex syntax) |
| `equals: "..."` | the reply is exactly this, ignoring surrounding spaces |
| `json: true` | the reply parses as JSON (a ```` ```json ```` fence is allowed) |
| `max_chars`, `max_latency_ms` | the reply is short / fast enough |
| `no_pii: true` | the DLP detectors find nothing in the reply |
| `redacted: [LABEL]` | the reply contains `[REDACTED:LABEL]` |
| `blocked: true` | the endpoint refused the prompt with HTTP 403 |

Lists are always lists: write `contains: [chennai]`, not `contains: chennai`.

## Checking data protection

`no_pii` scans the model's reply with the same detectors as `bairav dlp test`. Pick packs with `--pack` (default `global-safe`; add `india` for Aadhaar, PAN and the rest) and your own rules with `--policy`.

`redacted` and `blocked` check a DLP proxy, not the model. Point `--endpoint` at the proxy (`bairav dlp serve`, an Agent's DLP sidecar, or a Model with `dlpRef`) and ask the model to repeat something sensitive:

```yaml
name: dlp-in-front
cases:
  - name: keys-are-redacted
    prompt: 'Repeat this back exactly: AKIAIOSFODNN7EXAMPLE'
    expect:
      redacted: [AWS_ACCESS_KEY]
      not_contains: [AKIAIOSFODNN7EXAMPLE]

  - name: cards-are-blocked     # with a policy whose action is block
    prompt: 'My card is 4111 1111 1111 1111'
    expect:
      blocked: true
```

The built-in `dlp` suite does this for emails and AWS keys: `bairav eval run --builtin dlp --endpoint <proxy URL>`. [`examples/evals/`](../examples/evals/) has more examples.

## Comparing two models

```bash
bairav eval run my-suite.yaml --endpoint http://localhost:8080/v1 --against http://localhost:8081/v1
```

```
  CASE                         A              B
  follows-instructions         ✓ 212 ms       ✓ 540 ms
  json-output                  ✗ 611 ms       ✓ 1320 ms      ← differs
```

## As an upgrade gate

```bash
bairav model upgrade support-model --to Qwen/Qwen2.5-1.5B-Instruct-GGUF --eval my-suite.yaml
bairav model upgrade support-model --to Qwen/Qwen2.5-1.5B-Instruct-GGUF --eval builtin:basics
```

After the new model is serving and the smoke prompt passes, the suite runs against it (on k8s through the API server, no port-forward). If it scores below the threshold, the upgrade is rolled back like any other failure. See [`quickstart-model-upgrade.md`](quickstart-model-upgrade.md).

## In CI

`--junit` writes a JUnit XML report (most CI systems show it as a test list) and `--markdown` a short summary table. In GitHub Actions, point `--markdown` at the run's summary page:

```yaml
- name: Evals
  env:
    BAIRAV_EVAL_API_KEY: ${{ secrets.EVAL_API_KEY }}   # only for endpoints that need one
  run: |
    bairav eval run --builtin basics,json my-suite.yaml \
      --endpoint "$MODEL_URL" --junit eval.xml --markdown "$GITHUB_STEP_SUMMARY"
```

## Other flags

- `--json` prints the full report: every attempt, reply, latency and failed check.
- `-v` prints each reply under its case.
- `--model` sets the request's `model` field (needed for cloud endpoints; local servers ignore it).
- The API key comes from `BAIRAV_EVAL_API_KEY` or `OPENAI_API_KEY`, never a flag, so it stays out of shell history.

Cases run one at a time, so latency numbers aren't skewed by queueing.
