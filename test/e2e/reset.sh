#!/usr/bin/env bash
# Put the demo back to the state before step 1: the support namespace without
# injection, a plain curl pod, and its CredentialSource and CredentialBinding.
source "$(dirname "$0")/lib.sh"

credentials() {
  if [ "$E2E_PROVIDER" = fake ]; then
    cat <<'EOF'
apiVersion: core.riptides.io/v1alpha1
kind: CredentialSource
metadata: {name: fake-provider, namespace: support}
spec:
  rfc8693:
    tokenEndpointUrl: https://sts.fake.waterwheel.test/token
---
apiVersion: core.riptides.io/v1alpha1
kind: CredentialBinding
metadata: {name: support-bot-fake, namespace: support}
spec:
  workloadID: ns/support/sa/support-bot
  credentialSource: fake-provider
  propagation:
    injection:
      destinations: [{host: api.fake.waterwheel.test}]
EOF
    return
  fi

  [ -f "$ROOT/providers.env" ] || die "E2E_PROVIDER=anthropic needs providers.env (see docs/milestone-1.md)"
  set -a
  # shellcheck disable=SC1091
  source "$ROOT/providers.env"
  set +a
  cat <<EOF
apiVersion: core.riptides.io/v1alpha1
kind: CredentialSource
metadata: {name: anthropic, namespace: support}
spec:
  anthropic:
    organizationId: ${ANTHROPIC_ORGANIZATION_ID:?}
    workspaceId: ${ANTHROPIC_WORKSPACE_ID:-default}
    serviceAccountId: ${ANTHROPIC_SERVICE_ACCOUNT_ID:?}
    federationRuleId: ${ANTHROPIC_FEDERATION_RULE_ID:?}
---
apiVersion: core.riptides.io/v1alpha1
kind: CredentialBinding
metadata: {name: support-bot-anthropic, namespace: support}
spec:
  workloadID: ns/support/sa/support-bot
  credentialSource: anthropic
  propagation:
    injection:
      destinations: [{host: api.anthropic.com}]
EOF
}

kubectl delete namespace support --ignore-not-found --wait=true >/dev/null
sed "s|APP_IMAGE|$APP_IMAGE|" "$E2E/manifests/support-bot.yaml" | kubectl apply -f - >/dev/null
credentials | kubectl apply -f - >/dev/null
kubectl -n support rollout status deployment/support-bot --timeout=2m >/dev/null
log "reset: $(kubectl -n support get pods --no-headers | awk '{print $1" "$2}' | paste -sd ',' - | sed 's/,/, /g')"
