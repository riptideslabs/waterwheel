#!/usr/bin/env bash
# Create the kind cluster (once) and write .state/kubeconfig.
source "$(dirname "$0")/lib.sh"

if ! vm "$KIND" get clusters 2>/dev/null | grep -qx "$CLUSTER_NAME"; then
  log "creating kind cluster $CLUSTER_NAME"
  vm "$KIND" create cluster --name "$CLUSTER_NAME" --config hack/kind/cluster.yaml --wait 120s
fi

# The nodes pull images themselves (SPIRE's, for one), so they must trust
# EXTRA_ROOTS_FILE too, e.g. a TLS-intercepting proxy's.
if [ -n "${EXTRA_ROOTS_FILE:-}" ]; then
  crt=/usr/local/share/ca-certificates/waterwheel-extra-roots.crt
  for node in $(vm "$KIND" get nodes --name "$CLUSTER_NAME"); do
    [ "$(vm docker exec "$node" cat "$crt" 2>/dev/null)" != "$(cat "$EXTRA_ROOTS_FILE")" ] || continue
    log "trusting EXTRA_ROOTS_FILE in $node"
    vm docker exec -i "$node" sh -c "cat >$crt && update-ca-certificates >/dev/null 2>&1 && systemctl restart containerd" \
      <"$EXTRA_ROOTS_FILE"
  done
fi

mkdir -p "$STATE"
vm "$KIND" get kubeconfig --name "$CLUSTER_NAME" > "$KUBECONFIG"

for _ in $(seq 30); do kubectl get --raw /readyz >/dev/null 2>&1 && break; sleep 1; done
kubectl get --raw /readyz >/dev/null || die "the API server isn't reachable (Lima port forward?)"
log "cluster ready; KUBECONFIG=$KUBECONFIG"
