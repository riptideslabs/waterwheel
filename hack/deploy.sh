#!/usr/bin/env bash
# Install or upgrade Waterwheel in the kind cluster with the locally built
# images. IDENTITY_SOURCE=spire installs SPIRE first.
source "$(dirname "$0")/lib.sh"

source="${IDENTITY_SOURCE:-serviceAccountToken}"
[ "$source" != spire ] || "$ROOT/hack/spire.sh"

# Helm installs CRDs only on first install; apply them so changes land. Helm
# also claims their fields then, so take them over.
kubectl apply --server-side --force-conflicts -f "$ROOT/charts/waterwheel/crds" >/dev/null

# EXTRA_ROOTS_FILE: CAs the proxies must trust too, e.g. a TLS-intercepting
# proxy's between the cluster and the internet.
extra=()
[ -z "${EXTRA_ROOTS_FILE:-}" ] || extra=(--set-file "extraRoots=$EXTRA_ROOTS_FILE")

helm upgrade --install waterwheel "$ROOT/charts/waterwheel" \
  --namespace waterwheel-system --create-namespace "${extra[@]}" \
  --set identity.source="$source" \
  --set trustDomain="${TRUST_DOMAIN:-waterwheel.test}" \
  --set issuer="${JWT_ISSUER:-https://oidc.waterwheel.test}" \
  --set image.registry= \
  --set image.tag="${IMAGE_TAG:-dev}" \
  --wait --timeout 3m

# Dev images keep their tag, so Helm sees no change when only the image did.
kubectl -n waterwheel-system rollout restart deployment/waterwheel-controller >/dev/null
kubectl -n waterwheel-system rollout status deployment/waterwheel-controller --timeout=2m >/dev/null
