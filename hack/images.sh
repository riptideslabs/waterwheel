#!/usr/bin/env bash
# Cross-compile the binaries for the Docker host, build the images there and
# load them into kind.
source "$(dirname "$0")/lib.sh"

arch=$(vm_arch)
out="$STATE/images/linux/$arch"
mkdir -p "$out"

for bin in waterwheel-agent waterwheel-controller waterwheel-proxy; do
  log "building $bin (linux/$arch)"
  (cd "$ROOT" && GOOS=linux GOARCH="$arch" CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o "$out/$bin" "./cmd/$bin")
done
cp "$ROOT/build/Dockerfile" "$STATE/images/"

tag="${IMAGE_TAG:-dev}"
for bin in waterwheel-agent waterwheel-controller waterwheel-proxy; do
  vm docker build -q --target "$bin" -t "$bin:$tag" .state/images >/dev/null
done

log "loading images into kind"
vm "$KIND" load docker-image --name "$CLUSTER_NAME" "waterwheel-agent:$tag" "waterwheel-controller:$tag" "waterwheel-proxy:$tag"
