# The same as curl.sh, for images without a shell (distroless), with Python's
# ssl module and its default verify paths.
import json
import os
import ssl
import urllib.request

BODY = {"model": "m", "max_tokens": 10, "messages": [{"role": "user", "content": "hi"}]}


def ask():
    req = urllib.request.Request(
        "https://api.fake.waterwheel.test/v1/messages",
        data=json.dumps(BODY).encode(),
        headers={"anthropic-version": "2023-06-01", "content-type": "application/json"},
    )
    with urllib.request.urlopen(req, context=ssl.create_default_context()) as resp:
        print(resp.read().decode())


ask()
for name in ("SSL_CERT_FILE", "CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE", "NODE_EXTRA_CA_CERTS",
             "AWS_CA_BUNDLE", "GRPC_DEFAULT_SSL_ROOTS_FILE_PATH"):
    os.environ.pop(name, None)
# Distroless has no /usr/lib/ssl/cert.pem: its image sets SSL_CERT_FILE to the
# OS bundle instead, so go back to that.
os.environ["SSL_CERT_FILE"] = "/etc/ssl/certs/ca-certificates.crt"
ask()
