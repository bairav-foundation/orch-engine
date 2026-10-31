#!/usr/bin/env bash
# Pin every GitHub Action in .github/workflows to a full commit SHA, keeping
# the human-readable ref as a comment (Dependabot updates both):
#
#   uses: actions/checkout@v4   ->   uses: actions/checkout@<40-hex sha> # v4
#
#   scripts/pin-actions.sh           rewrite the workflows (needs `gh` logged in)
#   scripts/pin-actions.sh --check   no network; exit 1 if anything is unpinned (CI)
#
# Why: a tag like @v4 can be moved by whoever controls that repo; a commit
# SHA can't. Local actions (./...) and docker:// references are left alone.
#
# One exception: the SLSA generator (slsa-framework/slsa-github-generator)
# must be referenced by a release tag. It checks its own ref to prove which
# builder made the provenance, and doesn't support SHA pinning.
set -euo pipefail
cd "$(dirname "$0")/.."
mode=${1:-pin}
re='^([[:space:]]*-?[[:space:]]*uses:[[:space:]]*)([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)(/[^@[:space:]]*)?@([^[:space:]#]+)([[:space:]]*#.*)?$'
bad=0
for f in .github/workflows/*.yml .github/workflows/*.yaml; do
  [ -f "$f" ] || continue
  tmp=$(mktemp)
  while IFS= read -r line || [ -n "$line" ]; do
    if [[ $line =~ $re ]]; then
      pre=${BASH_REMATCH[1]} owner=${BASH_REMATCH[2]} repo=${BASH_REMATCH[3]} path=${BASH_REMATCH[4]} ref=${BASH_REMATCH[5]}
      if [[ ! $ref =~ ^[0-9a-f]{40}$ ]] && ! [[ $owner/$repo = slsa-framework/slsa-github-generator && $ref =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
        if [ "$mode" = "--check" ]; then
          echo "unpinned: $f: $owner/$repo$path@$ref" >&2; bad=1
        else
          label=$ref
          sha=$(gh api "repos/$owner/$repo/commits/$ref" --jq .sha 2>/dev/null || true)
          if [[ ! $sha =~ ^[0-9a-f]{40}$ ]]; then
            # No floating tag like "v2" (some actions only publish v2.4.3):
            # take the newest release in that line.
            label=$(gh api "repos/$owner/$repo/tags" --paginate --jq '.[].name' \
              | grep -E "^${ref//./\\.}\\." | sort -V | tail -1)
            [ -n "$label" ] && sha=$(gh api "repos/$owner/$repo/commits/$label" --jq .sha)
          fi
          [[ $sha =~ ^[0-9a-f]{40}$ ]] || { echo "could not resolve $owner/$repo@$ref" >&2; exit 1; }
          echo "pinned: $owner/$repo$path@$ref -> $sha ($label)"
          line="${pre}${owner}/${repo}${path}@${sha} # ${label}"
        fi
      fi
    fi
    printf '%s\n' "$line" >> "$tmp"
  done < "$f"
  if [ "$mode" = "--check" ]; then rm -f "$tmp"; else mv "$tmp" "$f"; fi
done
if [ "$mode" = "--check" ]; then
  [ "$bad" = 0 ] && echo "all actions pinned to commit SHAs" || { echo "run scripts/pin-actions.sh to pin them" >&2; exit 1; }
fi
