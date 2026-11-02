#!/usr/bin/env bash
# Step 5: a fake exfiltration script in the app container finds nothing to steal.
source "$(dirname "$0")/../lib.sh"
cd "$ROOT" || exit

title "5 · A fake exfiltration script"
expect "kubectl exec -i deploy/support-bot -c app -- sh < test/e2e/steps/exfil.sh" "Nothing to steal."
