# Shared by the dev and e2e scripts.
# shellcheck shell=bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STATE="$ROOT/.state"
CLUSTER_NAME="${CLUSTER_NAME:-waterwheel}"

# The Lima VM that runs Docker. Set LIMA_VM= (empty) to use Docker and kind on
# this machine, as CI does.
LIMA_VM="${LIMA_VM-waterwheel-dev}"

if [ -n "$LIMA_VM" ]; then
  KIND="$STATE/bin/kind"
else
  KIND="${KIND:-kind}"
fi

export KUBECONFIG="$STATE/kubeconfig"

# Run a command where Docker runs: in the VM, in this directory (it's mounted
# at the same path), or right here.
vm() {
  if [ -n "$LIMA_VM" ]; then
    limactl shell --workdir "$ROOT" "$LIMA_VM" -- "$@"
  else
    (cd "$ROOT" && "$@")
  fi
}

# The Docker host's architecture, as GOARCH.
vm_arch() { vm uname -m | sed 's/aarch64/arm64/; s/x86_64/amd64/'; }

log() { printf '\033[1;34m==>\033[0m %s\n' "$*" >&2; }
die() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }
