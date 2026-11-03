#!/usr/bin/env bash
# Install SPIRE in the kind cluster from SPIFFE's Helm charts, for the spire
# identity source: the server with its controller manager, the agents, the CSI
# driver, and the OIDC discovery provider. Safe to run again.
#
# - SPIRE's default ClusterSPIFFEID is off: it gives every pod an identity,
#   which would bypass WorkloadIdentity selectors.
# - The OIDC discovery provider serves plain HTTP, inside the cluster only;
#   the e2e's fake provider reads SPIRE's keys there.
# - The release is spire in namespace spire-server, so the controller
#   manager's class is spire-server-spire, the chart's spire.className.
source "$(dirname "$0")/lib.sh"

repo=https://spiffe.github.io/helm-charts-hardened/

log "installing SPIRE (trust domain ${TRUST_DOMAIN:-waterwheel.test})"
helm upgrade --install spire-crds spire-crds --repo "$repo" --version 0.6.1 \
  --namespace spire-server --create-namespace --wait >/dev/null
helm upgrade --install spire spire --repo "$repo" --version 0.30.4 \
  --namespace spire-server \
  --set global.spire.clusterName="$CLUSTER_NAME" \
  --set global.spire.trustDomain="${TRUST_DOMAIN:-waterwheel.test}" \
  --set spire-server.controllerManager.identities.clusterSPIFFEIDs.default.enabled=false \
  --set spiffe-oidc-discovery-provider.tls.spire.enabled=false \
  --wait --timeout 5m >/dev/null
