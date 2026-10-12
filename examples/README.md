# `examples/` — reference skills and policies

Small, runnable examples that show Persona A and Persona B what the product feels like.

## Status

Day 0 placeholder. Reference skills ship Week 3 Day 17-18; example policies ship Week 2 Day 14.

## Planned contents

### `skills/code-reviewer/` — Week 3
A LangGraph agent. Input: a git diff. Output: a list of review comments with line-anchored suggestions. Calls the local Mistral 7B model through the DLP proxy.

### `skills/infra-optimizer/` — Week 3
A CrewAI crew. Input: output of `kubectl top pods --all-namespaces -A`. Output: resource-tuning recommendations (requests/limits) with rationale. Showcases the Persona B quick-win story.

### `policies/default.yaml` — Week 2
The shipped-by-default DLP policy. Blocks PII and secrets; alerts on DB snapshot patterns. Safe for any environment.

### `policies/strict-finserv.yaml` — Week 2
Blocks everything in `default.yaml` plus financial identifiers (IBAN, SWIFT, routing numbers). Target audience: banks, finserv.

### `policies/relaxed-dev.yaml` — Week 2
Alerts on everything but blocks nothing. For local dev loops where you want visibility without friction.

## Contribution guidance

Every example must include:
- A `README.md` that a newcomer can follow in under 5 minutes.
- A minimal input file so `bairav skill run ...` works without editing.
- A link back to the sprint doc (`planning/30_DAY_ENGINEERING_SPRINT.md`) for context.
