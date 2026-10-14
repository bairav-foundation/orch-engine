# Examples

| Path | What it is |
|------|------------|
| [`mcp-echo/`](mcp-echo/) | A tiny MCP server (Python, HTTP + SSE) to try `bairav skill deploy` with. Image: `bairav/mcp-echo`. |
| [`dlp-policy.yaml`](dlp-policy.yaml) | A commented DLP policy file showing custom rules, checksums and per-rule actions. |
| [`policies/india.yaml`](policies/india.yaml) | Rules for common Indian identifiers: Aadhaar, PAN, GSTIN and phone numbers. |
| [`evals/`](evals/) | `bairav eval` suites: `basics.yaml` for a chat model, `dlp.yaml` for a DLP proxy in front of one. |

Try a policy without running anything:

```bash
bairav dlp test --policy examples/policies/india.yaml "my PAN is ABCDE1234F"
```

New examples should come with a README that someone can follow in five minutes.
