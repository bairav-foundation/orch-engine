# Security policy

Orch Engine handles prompts, model traffic and provider API keys, so security reports get priority.

**Report privately:** email security@bairavs.com, or use GitHub's "Report a vulnerability" on this repository (Security tab). Please don't open a public issue for a vulnerability.

Include what you found, how to reproduce it, and the version (`bairav version`). You'll get an acknowledgement within 3 days and a fix or a plan within 14 days for confirmed issues. We credit reporters in the release notes unless you'd rather stay anonymous.

**In scope:** the `bairav` CLI, the operator, the DLP proxy and gateway (including DLP bypasses: PII or secrets reaching a model or provider when a policy says they shouldn't), and the published container images.

**Supported versions:** the latest release.
