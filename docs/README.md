# `docs/` — engineering documentation

Internal engineering docs for the BAIRAVS Orch Engine. User-facing docs (install guides, tutorials) will live on the website; this folder is for contributors and maintainers.

## Contents

| File | What it covers |
|------|----------------|
| [[CODING_STANDARDS.md]] | Go + Python style, error handling, naming, commit/PR etiquette |
| [[ARCHITECTURE.md]] | System architecture: components, data flow, CRDs, networking |

## Planned additions

As the product matures, this folder will grow to include:

- `OPERATIONS.md` — running the product in production (after Week 4)
- `SECURITY.md` — threat model and security posture (after DLP is real)
- `RELEASE_PROCESS.md` — how we cut and ship releases (Week 4)
- `ADR/` — Architecture Decision Records, one file per significant decision

## How to navigate

Start with `ARCHITECTURE.md` if you're new — it gives the mental model. Then read `CODING_STANDARDS.md` before your first PR.

For product-level context, read the repo root `README.md`, then [`HISTORY.md`](../HISTORY.md).
