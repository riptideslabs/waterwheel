#!/usr/bin/env bash
# Step 3: turn Waterwheel on for the namespace. The webhook adds waterwheel-proxy to
# pods created from then on, with the pod's projected service account tokens.
source "$(dirname "$0")/../lib.sh"
cd "$ROOT" || exit

title "3 · Turn on Waterwheel for the pod"
run "kubectl label namespace support waterwheel.riptides.io/inject=enabled"
run "kubectl rollout restart deployment/support-bot"
run "kubectl rollout status deployment/support-bot"
wait_single_pod support-bot
expect "kubectl get pods" "2/2"
