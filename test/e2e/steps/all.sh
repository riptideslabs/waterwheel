#!/usr/bin/env bash
# The five steps back to back. With PAUSE=1, waits for a key before each command.
source "$(dirname "$0")/../lib.sh"

for s in "$E2E"/steps/[1-5]-*.sh; do
  "$s"
done

printf '\n%s%s━━ all five steps passed (provider: %s, identity: %s)%s\n' "$B" "$YELLOW" "$E2E_PROVIDER" "$E2E_IDENTITY" "$R"
