#!/usr/bin/env bash
# CA trust in common base images, after the five steps (make e2e). Each image's
# own client calls the fake API through the proxy twice: as injected, and again
# without the bundle env vars, so only the OS bundle paths the injector mounts
# over are left. The pods run in the demo's namespace as support-bot.
source "$(dirname "$0")/lib.sh"

[ "$E2E_PROVIDER" = fake ] || die "the trust check needs E2E_PROVIDER=fake"
# With SPIRE, only the support-bot Deployment's pods match its WorkloadIdentity.
[ "$E2E_IDENTITY" = serviceAccountToken ] || die "the trust check needs E2E_IDENTITY=serviceAccountToken"

# Name, image, and the check it runs. Alpine is the demo's own image.
images=(
  "debian mirror.gcr.io/library/buildpack-deps:trixie-curl curl"
  "ubuntu mirror.gcr.io/library/buildpack-deps:noble-curl curl"
  "fedora mirror.gcr.io/library/fedora:43 curl"
  "ubi9 registry.access.redhat.com/ubi9/ubi-minimal:9.7 curl"
  "amazonlinux mirror.gcr.io/library/amazonlinux:2023 curl"
  "suse registry.suse.com/bci/bci-base:16.0 curl"
  "distroless gcr.io/distroless/python3-debian13:latest python"
)

pod() {
  local cmd='["sh", "/checks/curl.sh"]'
  [ "$3" = python ] && cmd='["python3", "/checks/check.py"]'
  cat <<YAML
---
apiVersion: v1
kind: Pod
metadata: {name: trust-$1, namespace: support, labels: {app: trust}}
spec:
  serviceAccountName: support-bot
  automountServiceAccountToken: false
  enableServiceLinks: false
  restartPolicy: Never
  terminationGracePeriodSeconds: 2
  containers:
  - name: app
    image: $2
    imagePullPolicy: IfNotPresent
    command: $cmd
    volumeMounts: [{name: checks, mountPath: /checks}]
  volumes:
  - name: checks
    configMap: {name: trust-checks}
YAML
}

# Wait for a pod to finish, and print its phase.
finished() {
  local phase
  for _ in $(seq 600); do
    phase=$(kubectl -n support get pod "$1" -o jsonpath='{.status.phase}')
    case "$phase" in Succeeded | Failed) break ;; esac
    sleep 1
  done
  echo "$phase"
}

title "CA trust in base images"
kubectl label namespace support waterwheel.riptides.io/inject=enabled --overwrite >/dev/null
kubectl -n support delete pod -l app=trust --ignore-not-found --wait=true >/dev/null
kubectl -n support create configmap trust-checks --from-file="$E2E/trust" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
for entry in "${images[@]}"; do
  read -r name image check <<<"$entry"
  pod "$name" "$image" "$check"
done | kubectl apply -f - >/dev/null

for entry in "${images[@]}"; do
  read -r name image _ <<<"$entry"
  phase=$(finished "trust-$name")
  out=$(kubectl -n support logs "trust-$name" -c app 2>&1) || true
  if [ "$phase" != Succeeded ] || [ "$(grep -c 'Hello from the fake provider' <<<"$out")" != 2 ]; then
    printf '%sFAIL%s %s (%s): %s\n' "$RED" "$R" "$name" "$image" "${phase:-not started}" >&2
    {
      echo "--- app"
      printf '%s\n' "$out"
      echo "--- waterwheel-proxy log"
      kubectl -n support logs "trust-$name" -c waterwheel-proxy --tail=20 2>&1
      echo "--- events"
      kubectl -n support get events --field-selector "involvedObject.name=trust-$name" 2>&1 | tail -5
    } | sed 's/^/  /' >&2
    exit 1
  fi
  printf '  ✓ %s %s(%s)%s\n' "$name" "$DIM" "$image" "$R"
done

kubectl -n support delete pod -l app=trust --wait=false >/dev/null
printf '\n%s%s━━ CA trust passed in %d base images, with and without the bundle env vars%s\n' \
  "$B" "$YELLOW" "${#images[@]}" "$R"
