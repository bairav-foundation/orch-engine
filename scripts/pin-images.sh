#!/usr/bin/env bash
# Pin every base image in our Dockerfiles to its current digest, keeping the
# tag for humans (Dependabot updates both):
#
#   FROM debian:bookworm-slim   ->   FROM debian:bookworm-slim@sha256:<digest>
#
#   scripts/pin-images.sh           rewrite (needs docker with buildx; public
#                                   images, no login needed)
#   scripts/pin-images.sh --check   no network; exit 1 if anything is unpinned
#
# The digest is the multi-arch index, so amd64 and arm64 builds both match.
set -euo pipefail
cd "$(dirname "$0")/.."
mode=${1:-pin}
files=(images/serving/Dockerfile images/dlp/Dockerfile operator/Dockerfile examples/mcp-echo/Dockerfile)
re='^([Ff][Rr][Oo][Mm][[:space:]]+)([^[:space:]@]+)(@sha256:[0-9a-f]{64})?([[:space:]].*)?$'
bad=0
for f in "${files[@]}"; do
  tmp=$(mktemp)
  while IFS= read -r line || [ -n "$line" ]; do
    if [[ $line =~ $re ]]; then
      pre=${BASH_REMATCH[1]} img=${BASH_REMATCH[2]} dig=${BASH_REMATCH[3]} rest=${BASH_REMATCH[4]}
      # skip stage references (FROM builder) and scratch
      if [[ -z $dig && $img == *[:/]* ]]; then
        if [ "$mode" = "--check" ]; then
          echo "unpinned: $f: $img" >&2; bad=1
        else
          d=$(docker buildx imagetools inspect "$img" --format '{{json .Manifest.Digest}}' | tr -d '"')
          [[ $d =~ ^sha256:[0-9a-f]{64}$ ]] || { echo "could not resolve $img" >&2; exit 1; }
          echo "pinned: $img -> $d"
          line="${pre}${img}@${d}${rest}"
        fi
      fi
    fi
    printf '%s\n' "$line" >> "$tmp"
  done < "$f"
  if [ "$mode" = "--check" ]; then rm -f "$tmp"; else mv "$tmp" "$f"; fi
done
if [ "$mode" = "--check" ]; then
  [ "$bad" = 0 ] && echo "all base images pinned to digests" || { echo "run scripts/pin-images.sh to pin them" >&2; exit 1; }
fi
