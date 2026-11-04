# Waterwheel

Waterwheel turns a pod's identity into the API credentials it needs, so the
app does not need a long-lived API key. A sidecar exchanges the pod's
Kubernetes service account token for a short-lived provider token (Anthropic,
OpenAI, Google Cloud, Azure, RFC 7523, or a JSON-compatible RFC 8693 endpoint)
and adds it to the app's HTTPS requests. With SPIRE, it exchanges a JWT-SVID
instead, for the pod's `WorkloadIdentity`.

- `waterwheel-controller`: serves each sidecar its bindings and injects the
  sidecar.
- `waterwheel-proxy`: the per-pod CONNECT proxy that adds the credentials.
- `waterwheel-agent`: Waterwheel's own SPIFFE agent, a later identity source; not
  deployed by default.

**Status: early preview.** The controller, proxy, chart, and a kind end-to-end
demo are implemented, but there is no tagged Waterwheel release or published
production support claim yet. See [the design](docs/design.md) and
[the milestone notes](docs/milestone-1.md) for scope and known limits.

## Quickstart: run the five-step demo

This uses a fake token endpoint and API, so it needs no provider account or API
key. It creates a local kind cluster named `waterwheel` and verifies that the
same request fails without the proxy and succeeds after injection.

Prerequisites: Go 1.26 or newer, a running Docker daemon, kind, kubectl, Helm,
and jq. On macOS or Linux:

```bash
git clone https://github.com/riptideslabs/waterwheel.git
cd waterwheel
LIMA_VM= make e2e
```

The last line should say `all five steps passed (provider: fake, identity:
serviceAccountToken)`. The demo sets `automountServiceAccountToken: false` and
checks that the app container has no mounted service account token. To run the
SPIRE variant, use `LIMA_VM= E2E_IDENTITY=spire make e2e`. To remove the demo
cluster, run `LIMA_VM= make down`.

For a real provider, its federation rule must trust the cluster's issuer and
signing keys; [docs/oidc.md](docs/oidc.md) explains that setup. The demo's
`E2E_PROVIDER=anthropic` mode additionally needs Anthropic configuration; see
[docs/milestone-1.md](docs/milestone-1.md).

## Security model and current limits

Waterwheel mounts its projected identity tokens and optional SPIRE socket only
into the sidecar. It leaves Kubernetes' default service account token mount
unchanged. Set `automountServiceAccountToken: false` on the workload if the app
must have no default token mount; the demo does this. The sidecar **does** hold
a projected token (or obtain a JWT-SVID), keeps the exchanged provider token
in memory, and holds a per-pod TLS CA key. Someone who compromises the app can
still use its permitted API access through the proxy; Waterwheel reduces
long-lived key exposure, not misuse of an authorized workload. Pods sharing a
service account share its bindings unless SPIRE selectors distinguish them.

Today, intercepted destinations use HTTP/1.1; gRPC to a bound host is not
supported. A binding that needs a new token audience requires a pod restart.
CA bundle mounting has been exercised with the demo's Alpine image, not every
application image. See [the design](docs/design.md) for details and
[SECURITY.md](SECURITY.md) for private vulnerability reports.

## Development

```bash
make dev        # Lima VM waterwheel-dev, kind cluster waterwheel, images, Helm install
make generate   # deep copies and CRDs after changing api/v1alpha1
make lint test
make e2e        # the KCD demo's five steps on a fresh cluster, checked
make demo       # the same steps, waiting for a key before each command
make site       # the project page (site/), at http://localhost:4321
```

`make e2e` runs against an offline fake provider. `E2E_PROVIDER=anthropic`
uses the real API with the IDs in a gitignored `providers.env`; see
[docs/milestone-1.md](docs/milestone-1.md). `E2E_IDENTITY=spire` installs
SPIRE and runs the same steps with JWT-SVIDs.

`LIMA_VM= make dev` uses Docker and kind on the local machine instead of Lima.
Behind a TLS-intercepting proxy (like a Riptides workstation with MITM on),
pass its CA, so that the VM's Docker and the sidecars trust it:
`EXTRA_ROOTS_FILE="$HOME/Library/Application Support/riptides/mitm-ca.pem" make e2e`
(or `make dev`).
`kubectl` uses `.state/kubeconfig`.

## License

Apache-2.0.
