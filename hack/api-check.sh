#!/usr/bin/env bash
# Server-side dry runs of the API samples: every file in test/api/valid must be
# accepted, and every file in test/api/invalid rejected with the text on its
# "# expect:" line. Needs a cluster with Waterwheel installed (make deploy).
source "$(dirname "$0")/lib.sh"

kubectl create namespace api-check --dry-run=client -o yaml | kubectl apply -f - >/dev/null

failed=0
pass() { printf '  \033[32mok\033[0m    %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m  %s: %s\n' "$1" "$2"; failed=1; }

for f in "$ROOT"/test/api/valid/*.yaml; do
  name=${f#"$ROOT"/}
  if out=$(kubectl apply --dry-run=server -f "$f" 2>&1); then
    pass "$name"
  else
    fail "$name" "rejected: $out"
  fi
done

for f in "$ROOT"/test/api/invalid/*.yaml; do
  name=${f#"$ROOT"/}
  want=$(sed -n 's/^# expect: //p' "$f")
  [ -n "$want" ] || { fail "$name" "no '# expect:' line"; continue; }
  if out=$(kubectl apply --dry-run=server -f "$f" 2>&1); then
    fail "$name" "accepted"
  elif [[ $out != *"$want"* ]]; then
    fail "$name" "rejected, but without '$want': $out"
  else
    pass "$name"
  fi
done

exit "$failed"
