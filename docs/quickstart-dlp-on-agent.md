# Quickstart — DLP on an Agent

With a DLP policy attached, an Agent's outbound Model calls flow through a per-Model **DLP proxy sidecar** that inspects both the prompt going out AND the completion coming back. Your agent code doesn't change — it still reads `BAIRAV_MODEL_<X>_ENDPOINT` — but the endpoint now points at a localhost sidecar that redacts (or blocks) sensitive spans before either side of the wire sees them.

Same operator, same CRD group. New concept: `DLPPolicy` — a reusable, first-class K8s resource that Agents reference via `spec.dlpRef`.

---

## The mental model

```
┌───────────────────────────────────────────────────────────┐
│                    Agent Pod                              │
│                                                           │
│   ┌───────────┐   BAIRAV_MODEL_QWEN_ENDPOINT              │
│   │  Agent    ├─────────────► http://127.0.0.1:9100/v1    │
│   │ container │                                           │
│   └───────────┘                    │                      │
│                                    ▼                      │
│                       ┌────────────────────────┐          │
│                       │  dlp-qwen (sidecar)    │          │
│                       │  scans req + response  │          │
│                       │  action: redact        │          │
│                       └────────────┬───────────┘          │
└────────────────────────────────────┼──────────────────────┘
                                     ▼
                    http://qwen.default.svc.cluster.local:8080/v1
                              (Model Service)
```

Every request goes: **Agent → localhost sidecar → Model Service**. Every response returns via the same path. The sidecar is bounded to one Model — with two ModelRefs you get two sidecars, on ports 9100 and 9101.

---

## Full walkthrough

**1. Create a DLPPolicy.**

```yaml
# prod-guardrails.yaml
apiVersion: orch.bairavs.com/v1alpha1
kind: DLPPolicy
metadata:
  name: prod-guardrails
spec:
  packs:
    - global-safe          # Aadhaar, NHS, IBAN, etc — all checksum-anchored
    - india                # extra India-specific rules
  defaultAction: redact
  direction: both          # inspect prompts AND completions
  auditPath: "-"           # audit → stdout, picked up by kubectl logs
```

```bash
kubectl apply -f prod-guardrails.yaml
kubectl get dlp
# NAME              PHASE   DIRECTION   RULES   AGE
# prod-guardrails   Ready   both        8       10s
```

The reconciler validates every pack + custom rule, sets `.status.phase = Ready` when the policy is safe to reference. An invalid policy stays `Invalid` with a `Validated=False` Condition naming what's wrong.

**2. Deploy an Agent that references it.**

Two paths:

*Path A — kubectl.* Edit an existing Agent CR and add `spec.dlpRef`:

```yaml
apiVersion: orch.bairavs.com/v1alpha1
kind: Agent
metadata:
  name: support-agent
spec:
  image: ghcr.io/myorg/support-agent:v1
  modelRefs:
    - name: qwen2-5-0-5b
  dlpRef:
    name: prod-guardrails   # NEW — points at the DLPPolicy above
```

*Path B — CLI.*

```bash
bairav agent deploy ghcr.io/myorg/support-agent:v1 --target k8s \
  --model qwen2-5-0-5b --dlp-policy prod-guardrails
```

**3. Watch the Agent roll with the sidecar attached.**

```bash
kubectl get agents
# NAME            PHASE   IMAGE                              ENDPOINT
# support-agent   Ready   ghcr.io/myorg/support-agent:v1     http://support-agent.default.svc.cluster.local:8000

kubectl get pod -l orch.bairavs.com/agent=support-agent
# NAME                              READY   STATUS    RESTARTS   AGE
# support-agent-77bd5b9f6c-2k4rq    2/2     Running   0          20s
```

Note the `2/2` — one container is the agent, the other is the DLP sidecar (`dlp-qwen2-5-0-5b`). Confirm:

```bash
kubectl describe pod -l orch.bairavs.com/agent=support-agent | grep -A1 "Containers:"
# Containers:
#   agent:
#   dlp-qwen2-5-0-5b:
```

**4. Verify the endpoint rewrite.**

The `BAIRAV_MODEL_QWEN2_5_0_5B_ENDPOINT` env var inside the agent container now points at `http://127.0.0.1:9100/v1` — the sidecar — not the raw Model Service. Confirm:

```bash
kubectl exec -c agent deploy/support-agent -- env | grep BAIRAV_MODEL
# BAIRAV_MODEL_QWEN2_5_0_5B_ENDPOINT=http://127.0.0.1:9100/v1
```

Your agent code doesn't need to know — it already reads that env var. The DLP interposition is transparent.

**5. See it work.**

Trigger a prompt containing PII from the agent:

```bash
kubectl port-forward svc/support-agent 8000:8000
curl -s http://127.0.0.1:8000/chat \
  -d '{"message":"the customer aadhaar is 5678 1234 9017 and email is jane@corp.com"}'
```

Then tail the DLP sidecar's audit stream:

```bash
kubectl logs -f -c dlp-qwen2-5-0-5b deploy/support-agent | jq
# {"timestamp":"...","direction":"both","action":"redact","labels":["AADHAAR","EMAIL"],
#  "hit_count":2,"response_hits":0,"latency_ms":142}
```

Both the outbound prompt (prompt-side hits) and the completion (response-side hits when the model echoes back sensitive spans) are counted separately, so SOC dashboards can distinguish "PII heading TO the LLM" from "PII coming BACK from it."

---

## What happens when the policy is Invalid

Same story as a missing Model or Skill: the Agent stays in `WaitingForDeps` with a Condition naming the problem:

```bash
kubectl describe agent support-agent | tail -10
# Conditions:
#   Type: Progressing  Status: True   Reason: AwaitingDependencies
#   Message: invalid DLPPolicy: prod-guardrails (see its Validated condition)
```

Read the policy's own Condition to find the root cause:

```bash
kubectl describe dlp prod-guardrails
# Status:
#   Phase: Invalid
#   Conditions:
#     Type: Validated  Status: False  Reason: SpecInvalid
#     Message: customRules[0].regex does not compile: error parsing regexp
```

Fix the policy; the operator's Watches wake the Agent and it moves to Ready without needing a rollout.

---

## Custom rules

Inline `customRules` are projected by the operator into a ConfigMap named `dlppolicy-<policy>` (key `policy.yaml`, the same v1 format the CLI's `--policy` flag reads). Each sidecar mounts it at `/etc/bairav/policy` and gets `BAIRAV_DLP_POLICY` pointing at it.

```yaml
spec:
  packs: [global-safe]
  customRules:
    - label: TICKET_ID
      regex: 'TKT-\d{6}'
    - label: INTERNAL_IBAN
      regex: '\bGB\d{2}[A-Z]{4}\d{14}\b'
      action: block          # per-rule override; "audit" maps to the CLI's audit_only
```

Validation catches the collisions the sidecar would otherwise crash on: a custom label can't reuse a built-in (EMAIL, SSN, CREDIT_CARD, PHONE_US, AWS_ACCESS_KEY), a label from an enabled pack, or another custom rule.

Editing the policy rolls the Agent automatically. The Pod template carries an `orch.bairavs.com/dlp-policy-hash` annotation, so changing a rule restarts the sidecars with the new file. Without it, a ConfigMap edit alone would never restart the Pod.

A policy with `customRules` and no `packs` sets `BAIRAV_DLP_PACK=none` on the sidecar, so the CLI's `global-safe` default doesn't come back in. The always-on built-ins still apply.

## Streaming

`stream: true` responses on `/chat/completions` and `/completions` are redacted as they stream. The sidecar keeps one buffer per choice and holds back the last 128 bytes, so a span split across chunks (`jane@co` | `rp.com`) is still caught. Held text is flushed when the choice sends `finish_reason`, on `[DONE]`, or when the upstream closes. The cost is that text arrives about 128 bytes behind the model. The last token lands at the same time as before.

## Enforce at the model instead (no bypass)

The sidecar protects traffic from Agents that use it. Anything else in the cluster can still call the Model's Service directly. To close that, put the policy on the Model itself:

```bash
bairav model deploy Qwen/Qwen2.5-0.5B-Instruct-GGUF --target k8s \
  --namespace shared-models --name qwen --dlp-ref prod-guardrails
```

or in YAML, `spec.dlpRef: {name: prod-guardrails}` on the Model. The operator then:

- runs the DLP proxy inside the Model Pod on the Service port,
- binds `llama-server` to `127.0.0.1` only, so the proxy is the one way in,
- doesn't deploy the Model at all until the policy is Ready (fail closed); `kubectl describe model` shows why under the `DLPEnforced` condition.

The Service name, port and endpoint don't change, so callers need no changes. Agents can keep their own sidecar too; then traffic is checked twice, once per policy. A later `bairav model deploy` keeps the policy unless you pass `--dlp-ref ""`. Local models only: cloud models already run behind the DLP gateway.

Smoke test: `bash scripts/smoke-model-dlp.sh`.

## What's in and what's still out

**In:**
- `DLPPolicy` CRD: reusable, validated, with packs and inline customRules.
- One sidecar per Model ref, with the env-var endpoint rewritten transparently.
- Request-side redaction, plus response-side redaction for both buffered JSON and SSE streams.
- `bairav agent deploy --dlp-policy`.
- Model-side enforcement with `spec.dlpRef` on a Model (`bairav model deploy --dlp-ref`).
- Audit stream with `direction`, `response_hits`, and `response_labels`.

For tool calls, put a policy on the Skill instead: see [DLP on tool calls](./quickstart-skill.md#dlp-on-tool-calls).

**Not yet:**
- **Path-routed single sidecar.** N ModelRefs still means N sidecars.
- **Block on the response side.** Block-effective hits in a response get redacted instead, because the upstream has already sent a 200.

---

## Cross-reference

- [`quickstart-agent.md`](./quickstart-agent.md) — the base Agent walkthrough this builds on.
- [`quickstart-dlp.md`](./quickstart-dlp.md) — the original standalone DLP proxy (Compose target).
