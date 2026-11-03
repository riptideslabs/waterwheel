#!/usr/bin/env bash
# Step 4: the same command now succeeds. The subject token and the proxy's log
# show why: Kubernetes (or SPIRE) gave the pod its identity, tokenex exchanged
# it for the provider's token, and the request went out with that token.
source "$(dirname "$0")/../lib.sh"
cd "$ROOT" || exit

title "4 · The same command"
if [ "$E2E_PROVIDER" = fake ]; then
  expect "$ASK_CLAUDE" "Hello from the fake provider"
else
  expect_none "$ASK_CLAUDE" '"type":'
fi
if [ "$E2E_IDENTITY" = spire ]; then
  note "the pod's identity: a JWT-SVID from SPIRE, decoded"
else
  note "the pod's identity: its projected service account token, decoded"
fi
expect "kubectl exec deploy/support-bot -c waterwheel-proxy -- waterwheel-proxy token $AUDIENCE" "$SUBJECT"
note "the proxy's log: the bindings, the token from the provider, the request"
expect "kubectl logs deploy/support-bot -c waterwheel-proxy" "added token"
