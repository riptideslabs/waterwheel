#!/usr/bin/env bash
# Prepare the cluster for the e2e: the e2e CA, the fake provider and its DNS
# names, the test app image, and Waterwheel with the e2e CA as an extra root;
# with E2E_IDENTITY=spire, SPIRE too. Safe to run again.
source "$(dirname "$0")/lib.sh"

mkdir -p "$E2E_STATE"
(cd "$ROOT" && go run ./test/e2e/certgen "$E2E_STATE")

arch=$(vm_arch)

log "building the fake provider (linux/$arch) and the test curl image"
ctx="$STATE/images/fakeprovider"
mkdir -p "$ctx"
(cd "$ROOT" && GOOS=linux GOARCH="$arch" CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' \
  -o "$ctx/fakeprovider" ./test/e2e/fakeprovider)
cp "$E2E/fakeprovider/Dockerfile" "$ctx/"
vm docker build -q -t waterwheel-e2e-fakeprovider:dev .state/images/fakeprovider >/dev/null

ctx="$STATE/images/e2e-curl"
mkdir -p "$ctx"
cp "$E2E/curl/Dockerfile" "$E2E_STATE/ca.crt" "$ctx/"
vm docker build -q -t waterwheel-e2e-curl:dev .state/images/e2e-curl >/dev/null

vm "$KIND" load docker-image --name "$CLUSTER_NAME" waterwheel-e2e-fakeprovider:dev waterwheel-e2e-curl:dev >/dev/null

# With SPIRE, the fake provider checks JWT-SVIDs against the keys SPIRE's OIDC
# discovery provider serves, as a real provider would.
jwks="" issuer=""
if [ "$E2E_IDENTITY" = spire ]; then
  jwks=http://spire-spiffe-oidc-discovery-provider.spire-server/keys
  issuer="https://oidc-discovery.$TRUST_DOMAIN"
fi

log "deploying the fake provider"
sed -e "s|\"SUBJECT\"|\"$SUBJECT\"|" -e "s|\"JWKS_URL\"|\"$jwks\"|" -e "s|\"ISSUER\"|\"$issuer\"|" \
  "$E2E/manifests/fakeprovider.yaml" | kubectl apply -f - >/dev/null
kubectl -n waterwheel-e2e create secret tls fake-provider-tls \
  --cert="$E2E_STATE/tls.crt" --key="$E2E_STATE/tls.key" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
# Pick up a rebuilt image (the tag doesn't change).
kubectl -n waterwheel-e2e rollout restart deployment/fake-provider >/dev/null
kubectl -n waterwheel-e2e rollout status deployment/fake-provider --timeout=2m >/dev/null

corefile=$(kubectl -n kube-system get configmap coredns -o jsonpath='{.data.Corefile}')
if ! grep -q 'fake\\.waterwheel\\.test' <<<"$corefile"; then
  log "sending *.fake.waterwheel.test to the fake provider"
  patched=$(awk '{ print } /^    ready$/ {
    print "    rewrite name regex (.*)\\.fake\\.waterwheel\\.test fake-provider.waterwheel-e2e.svc.cluster.local answer auto"
  }' <<<"$corefile")
  kubectl -n kube-system create configmap coredns --from-literal=Corefile="$patched" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null 2>&1
  kubectl -n kube-system rollout restart deployment/coredns >/dev/null
  kubectl -n kube-system rollout status deployment/coredns --timeout=2m >/dev/null
fi

# The e2e CA, plus the caller's extra roots (e.g. a TLS-intercepting proxy's
# between the cluster and the internet), become waterwheel-extra-roots.
cat "$E2E_STATE/ca.crt" ${EXTRA_ROOTS_FILE:+"$EXTRA_ROOTS_FILE"} >"$E2E_STATE/extra-roots.pem"
EXTRA_ROOTS_FILE="$E2E_STATE/extra-roots.pem" IDENTITY_SOURCE="$E2E_IDENTITY" TRUST_DOMAIN="$TRUST_DOMAIN" \
  "$ROOT/hack/deploy.sh"

# The stage commands run in the demo's namespace.
kubectl config set-context --current --namespace=support >/dev/null
log "e2e setup done (provider: $E2E_PROVIDER, identity: $E2E_IDENTITY)"
