#!/usr/bin/env bash
# Create the kind cluster (once) and write .state/kubeconfig.
source "$(dirname "$0")/lib.sh"

if ! vm "$KIND" get clusters 2>/dev/null | grep -qx "$CLUSTER_NAME"; then
  log "creating kind cluster $CLUSTER_NAME"
  vm "$KIND" create cluster --name "$CLUSTER_NAME" --config hack/kind/cluster.yaml --wait 120s
fi

mkdir -p "$STATE"
vm "$KIND" get kubeconfig --name "$CLUSTER_NAME" > "$KUBECONFIG"

for _ in $(seq 30); do kubectl get --raw /readyz >/dev/null 2>&1 && break; sleep 1; done
kubectl get --raw /readyz >/dev/null || die "the API server isn't reachable (Lima port forward?)"
log "cluster ready; KUBECONFIG=$KUBECONFIG"
