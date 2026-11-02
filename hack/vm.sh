#!/usr/bin/env bash
# Create (once) and start the Lima VM that runs Docker for kind, and build kind for it.
source "$(dirname "$0")/lib.sh"

[ -n "$LIMA_VM" ] || { log "LIMA_VM is empty: using Docker on this machine"; exit 0; }

if ! limactl list -q | grep -qx "$LIMA_VM"; then
  log "creating Lima VM $LIMA_VM"
  # Provisioning downloads Docker, so trust EXTRA_ROOTS_FILE from the first boot.
  set -- --set ".mounts += [{\"location\": \"$ROOT\", \"writable\": true}]"
  if [ -n "${EXTRA_ROOTS_FILE:-}" ]; then
    set -- "$@" --set ".caCerts.files += [\"$EXTRA_ROOTS_FILE\"]"
  fi
  limactl create --name="$LIMA_VM" --tty=false "$ROOT/hack/lima/waterwheel-dev.yaml" "$@"
fi

if [ "$(limactl list --format '{{.Status}}' "$LIMA_VM")" != Running ]; then
  log "starting Lima VM $LIMA_VM"
  limactl start --tty=false "$LIMA_VM"
fi

# Lima rewrites daemon.json at boot without restarting Docker; make the
# containerdSnapshotter=false setting take effect.
if vm docker info --format '{{.DriverStatus}}' | grep -q containerd.snapshotter; then
  log "switching Docker to the overlay2 image store"
  vm sudo systemctl restart docker
fi

# Behind a TLS-intercepting proxy (EXTRA_ROOTS_FILE), the VM's Docker must
# trust its CA to pull images.
# The VM sees the repo, so pass the file through .state.
if [ -n "${EXTRA_ROOTS_FILE:-}" ]; then
  mkdir -p "$STATE"
  cp "$EXTRA_ROOTS_FILE" "$STATE/vm-extra-roots.crt"
fi

if [ -n "${EXTRA_ROOTS_FILE:-}" ] && ! vm cmp -s .state/vm-extra-roots.crt /usr/local/share/ca-certificates/waterwheel-extra-roots.crt; then
  log "adding $EXTRA_ROOTS_FILE to the VM's trusted CAs"
  vm sudo install -m 0644 .state/vm-extra-roots.crt /usr/local/share/ca-certificates/waterwheel-extra-roots.crt
  vm sudo update-ca-certificates >/dev/null
  vm sudo systemctl restart docker
fi

if [ ! -x "$KIND" ]; then
  log "building kind for the VM"
  arch=$(vm_arch)
  GOOS=linux GOARCH="$arch" CGO_ENABLED=0 GOBIN='' go install sigs.k8s.io/kind@v0.33.0
  mkdir -p "$STATE/bin"
  cp "$(go env GOPATH)/bin/linux_$arch/kind" "$KIND"
fi
