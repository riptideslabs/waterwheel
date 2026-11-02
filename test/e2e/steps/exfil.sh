# shellcheck shell=sh
# A pretend attacker with a shell in the app container looks for credentials.
# Runs inside the pod (busybox sh): kubectl exec -i ... -- sh < test/e2e/steps/exfil.sh

found=0
check() { printf '\n\033[1m▶ %s\033[0m\n' "$1"; }
ok() { printf '  \033[32m✓ %s\033[0m\n' "$1"; }
hit() { printf '  \033[31m✗ %s\033[0m\n' "$1"; found=1; }

check "environment variables that look like credentials"
creds=$(env | grep -iE 'key|token|secret|passw|auth|bearer' | cut -d= -f1)
if [ -n "$creds" ]; then hit "$creds"; else ok "none ($(env | wc -l) variables)"; fi

check "files with an Anthropic key or token (sk-ant-...) or a JWT"
# shellcheck disable=SC2038 # busybox: the demo version, file names here are plain
files=$(find / \( -path /proc -o -path /sys -o -path /dev \) -prune -o -type f -size -4M -print 2>/dev/null |
  xargs grep -lsE 'sk-ant-[a-z0-9]+-|eyJ[A-Za-z0-9_-]{10,}\.eyJ' 2>/dev/null)
if [ -n "$files" ]; then hit "$files"; else ok "none in $(find / \( -path /proc -o -path /sys \) -prune -o -type f -print 2>/dev/null | wc -l) files"; fi

check "Kubernetes service account token and Secret volumes"
if [ -e /var/run/secrets ]; then hit "$(find /var/run/secrets -type f)"; else ok "nothing under /var/run/secrets"; fi

check "the sidecar's projected tokens (to exchange them myself)"
if [ -e /run/waterwheel-proxy/tokens ]; then hit "$(ls /run/waterwheel-proxy/tokens)"; else ok "not mounted in this container"; fi

check "the proxy's memory"
ok "can't see it, only my own processes: $(ps -o comm= | sort -u | tr '\n' ' ')"

check "what is mounted"
ok "$(ls /run/waterwheel-proxy): the CA bundle, public certificates"

echo
if [ "$found" = 0 ]; then
  printf '\033[1;32mNothing to steal.\033[0m\n'
else
  printf '\033[1;31mFound something.\033[0m\n'
fi
