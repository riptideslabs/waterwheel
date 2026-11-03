#!/usr/bin/env bash
# Step 1: the client pod has no key: not in env, not in files, no Secrets.
source "$(dirname "$0")/../lib.sh"
cd "$ROOT" || exit

title "1 · The pod has no key"
run "kubectl get pods"
if [ "$E2E_IDENTITY" = spire ]; then
  note "Waterwheel runs next to it: one controller; SPIRE's agents give pods their identities"
else
  note "Waterwheel runs next to it: one controller, no agents on the nodes"
fi
expect "kubectl get pods -n waterwheel-system" "waterwheel-controller"
expect "kubectl get secrets" "No resources found"
expect_none "kubectl exec deploy/support-bot -c app -- env | cut -d= -f1" 'key|token|secret|passw|auth|bearer'
expect "kubectl exec deploy/support-bot -c app -- ls /var/run/secrets" "No such file"
