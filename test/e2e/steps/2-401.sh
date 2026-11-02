#!/usr/bin/env bash
# Step 2: calling the API from the pod fails with 401.
source "$(dirname "$0")/../lib.sh"
cd "$ROOT" || exit

title "2 · Call Claude from the pod"
expect "$ASK_CLAUDE" "authentication_error"
