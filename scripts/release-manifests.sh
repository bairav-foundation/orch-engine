#!/usr/bin/env bash
set -euo pipefail
# Renders a single install.yaml for a release: namespace, CRDs, RBAC and the
# operator Deployment, with the local :laptop images swapped for the
# published ghcr.io/bairav-foundation images at VERSION.
#
#   scripts/release-manifests.sh v0.2.0 > install.yaml
#   kubectl apply -f install.yaml
VERSION="${1:?usage: release-manifests.sh vX.Y.Z}"
REG="ghcr.io/bairav-foundation"
cd "$(dirname "$0")/.."

echo "# BAIRAVS Orch Engine ${VERSION}: CRDs, RBAC and operator."
echo "# kubectl apply -f install.yaml"
cat <<NS
---
apiVersion: v1
kind: Namespace
metadata:
  name: bairav-system
NS
for f in operator/config/crd/bases/*.yaml operator/config/rbac/*.yaml operator/config/manager/*.yaml; do
  echo "---"
  echo "# Source: ${f}"
  sed -e "s#bairav/operator:laptop#${REG}/operator:${VERSION}#g" \
      -e "s#bairav/serving:laptop#${REG}/serving:${VERSION}#g" \
      -e "s#bairav/dlp:laptop#${REG}/dlp:${VERSION}#g" \
      "$f"
done
