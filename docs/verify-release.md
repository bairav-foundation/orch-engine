# Verify what you install

Everything Orch Engine ships is built by one GitHub Actions workflow (`.github/workflows/release.yml`) from a tagged commit, and signed by that workflow with [Sigstore cosign](https://docs.sigstore.dev/) (keyless: the signature is tied to the workflow's GitHub identity, and there is no private key to steal). You can check each piece without trusting this website.

All examples use `v0.2.1`; use the version you're installing. You need [cosign](https://docs.sigstore.dev/cosign/system_config/installation/) 2.x.

The identity every signature must carry:

```bash
ID='^https://github.com/bairav-foundation/orch-engine/\.github/workflows/release\.yml@refs/tags/v'
ISSUER=https://token.actions.githubusercontent.com
```

## The CLI

`install.sh` does this for you (the signature check runs when cosign is installed). By hand:

```bash
V=v0.2.1
R=https://github.com/bairav-foundation/orch-engine/releases/download/$V
F=bairav_${V#v}_darwin_arm64.tar.gz          # pick your OS / CPU
curl -fsSLO $R/$F -O $R/checksums.txt -O $R/checksums.txt.sigstore.json

cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp "$ID" --certificate-oidc-issuer "$ISSUER"
shasum -a 256 -c checksums.txt --ignore-missing
```

`checksums.txt` covers every archive and its SBOM (`*.sbom.json`, SPDX), so one signature check covers all of them.

## The container images

The four images (`serving`, `dlp`, `operator`, `mcp-echo`) are signed by digest:

```bash
for i in serving dlp operator mcp-echo; do
  cosign verify ghcr.io/bairav-foundation/$i:v0.2.1 \
    --certificate-identity-regexp "$ID" --certificate-oidc-issuer "$ISSUER" > /dev/null && echo "$i ok"
done
```

Each image also carries an SBOM and build provenance (which workflow and commit built it):

```bash
docker buildx imagetools inspect ghcr.io/bairav-foundation/dlp:v0.2.1 --format '{{ json .SBOM }}'
docker buildx imagetools inspect ghcr.io/bairav-foundation/dlp:v0.2.1 --format '{{ json .Provenance }}'
```

In a cluster, deploy by digest rather than tag, so what runs can't change under you:

```bash
docker buildx imagetools inspect ghcr.io/bairav-foundation/operator:v0.2.1 --format '{{ json .Manifest.Digest }}'
```

To enforce signatures at admission time, point your policy engine (Kyverno `verifyImages`, Sigstore policy-controller, Ratify) at the identity above.

## How the project is run

What we do so that a compromised dependency or account is less likely to reach you:

- Releases come only from the release workflow on a `v*` tag. Release tags can't be moved or deleted, and only the maintainer can create them.
- `main` can't be force-pushed or deleted; outside changes need a pull request, green CI and maintainer review.
- Every GitHub Action in our workflows is pinned to a commit SHA and every base image to a digest, both checked in CI (`scripts/pin-actions.sh --check`, `scripts/pin-images.sh --check`). Dependabot proposes updates weekly; each goes through CI and review.
- CI runs `govulncheck` on the CLI and the operator, plus the full test suite with the race detector.
- Secret scanning with push protection is on for the repository.
- [OpenSSF Scorecard](https://scorecard.dev/viewer/?uri=github.com/bairav-foundation/orch-engine) grades these practices independently every week, and the project holds the [OpenSSF Best Practices](https://www.bestpractices.dev/projects/15882) passing badge.

Found a problem? See [SECURITY.md](../SECURITY.md).
