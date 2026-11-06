# shellcheck shell=sh
# Runs in each base image's app container: the fake API through the proxy with
# the image's own curl, as injected, then with only the OS bundle paths left.
set -eu

ask() {
  curl -sS --fail-with-body https://api.fake.waterwheel.test/v1/messages \
    -H 'anthropic-version: 2023-06-01' -H 'content-type: application/json' \
    -d '{"model": "m", "max_tokens": 10, "messages": [{"role": "user", "content": "hi"}]}'
  echo
}

ask
unset SSL_CERT_FILE CURL_CA_BUNDLE REQUESTS_CA_BUNDLE NODE_EXTRA_CA_CERTS AWS_CA_BUNDLE GRPC_DEFAULT_SSL_ROOTS_FILE_PATH
ask
