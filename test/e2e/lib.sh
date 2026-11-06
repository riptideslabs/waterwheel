# Shared by the e2e scripts: settings, and the stage helpers from the KCD demo
# with checks added. Each step prints its commands the way an audience should
# read them, and fails the run when an output isn't what the demo shows.
# shellcheck shell=bash
# shellcheck disable=SC2034 # the variables are for the scripts that source this
source "$(dirname "${BASH_SOURCE[0]}")/../../hack/lib.sh"

E2E="$ROOT/test/e2e"
E2E_STATE="$STATE/e2e"

# fake: the offline fake provider (CI). anthropic: the real API, with the IDs in
# providers.env and an issuer registered for this cluster.
E2E_PROVIDER="${E2E_PROVIDER:-fake}"
MODEL="${MODEL:-claude-opus-5-5}"

case "$E2E_PROVIDER" in
fake)
  API_HOST=api.fake.waterwheel.test
  AUDIENCE=https://sts.fake.waterwheel.test/token
  APP_IMAGE=waterwheel-e2e-curl:dev
  ;;
anthropic)
  API_HOST=api.anthropic.com
  AUDIENCE=https://api.anthropic.com
  APP_IMAGE=curlimages/curl:8.16.0
  ;;
*) die "E2E_PROVIDER must be fake or anthropic, not $E2E_PROVIDER" ;;
esac

# serviceAccountToken: the pod's projected token is its identity. spire: SPIRE
# gives it a JWT-SVID, for the WorkloadIdentity reset.sh creates.
E2E_IDENTITY="${E2E_IDENTITY:-serviceAccountToken}"
TRUST_DOMAIN="${TRUST_DOMAIN:-waterwheel.test}"

case "$E2E_IDENTITY" in
serviceAccountToken)
  WORKLOAD_ID=ns/support/sa/support-bot
  SUBJECT=system:serviceaccount:support:support-bot
  ;;
spire)
  WORKLOAD_ID=ns/support/support-bot
  SUBJECT="spiffe://$TRUST_DOMAIN/$WORKLOAD_ID"
  ;;
*) die "E2E_IDENTITY must be serviceAccountToken or spire, not $E2E_IDENTITY" ;;
esac

# --- stage output -----------------------------------------------------------

B=$'\033[1m' DIM=$'\033[2m' CYAN=$'\033[36m' YELLOW=$'\033[33m' RED=$'\033[1;31m' R=$'\033[0m'

title() { printf '\n%s%s━━ %s%s\n\n' "$B" "$YELLOW" "$*" "$R"; }
note() { printf '%s# %s%s\n' "$DIM" "$*" "$R"; }

# With PAUSE=1, wait for a key before each command (for live pacing).
pause() { if [ -n "${PAUSE:-}" ]; then read -rsn1 </dev/tty || true; fi; }

show() {
  printf '%s%s$ %s%s\n' "$B" "$CYAN" "$1" "$R"
  pause
}

# Print a command the way the audience should read it, then run it.
run() {
  show "$1"
  eval "$1" || true
  echo
}

# Like run, and the output must contain $2.
expect() {
  local out
  show "$1"
  out=$(eval "$1" 2>&1) || true
  printf '%s\n\n' "$out"
  grep -qF -- "$2" <<<"$out" || fail "expected \"$2\" in the output above"
}

# Like expect, retrying for up to 10 seconds, for what lands asynchronously.
expect_eventually() {
  local out
  show "$1"
  for _ in $(seq 20); do
    out=$(eval "$1" 2>&1) || true
    grep -qF -- "$2" <<<"$out" && break
    sleep 0.5
  done
  printf '%s\n\n' "$out"
  grep -qF -- "$2" <<<"$out" || fail "expected \"$2\" in the output above"
}

# Like run, and the output must not match the extended regexp $2.
expect_none() {
  local out
  show "$1"
  out=$(eval "$1" 2>&1) || true
  printf '%s\n\n' "$out"
  ! grep -qiE -- "$2" <<<"$out" || fail "the output above matches /$2/"
}

# Stop the run, with the logs that usually say why.
fail() {
  printf '%sFAIL%s %s\n' "$RED" "$R" "$*" >&2
  {
    echo "--- waterwheel-proxy log"
    kubectl -n support logs deploy/support-bot -c waterwheel-proxy --tail=20 2>&1
    echo "--- waterwheel-controller log"
    kubectl -n waterwheel-system logs deploy/waterwheel-controller --tail=10 2>&1
  } | sed 's/^/  /' >&2
  exit 1
}

# Wait until a deployment has exactly one pod left (old ones gone after a rollout).
wait_single_pod() {
  for _ in $(seq 60); do
    [ "$(kubectl -n support get pods -l "app=$1" --no-headers 2>/dev/null | wc -l | tr -d ' ')" = 1 ] && return 0
    sleep 0.5
  done
  die "old $1 pods did not go away"
}

# The request steps 2 and 4 send: plain curl in the app container, no key.
ASK_CLAUDE="kubectl exec deploy/support-bot -c app -- \\
    curl -sS --fail-with-body https://$API_HOST/v1/messages \\
      -H 'anthropic-version: 2023-06-01' -H 'content-type: application/json' \\
      -d '{\"model\": \"$MODEL\", \"max_tokens\": 100,
           \"messages\": [{\"role\": \"user\", \"content\": \"Say hello to KCD Budapest in one sentence.\"}]}' \\
  | jq '.error // .content[0].text'"
