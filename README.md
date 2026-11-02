# Waterwheel

Waterwheel turns a pod's identity into the API credentials it needs, so no pod
holds a long-lived key. A sidecar exchanges the pod's Kubernetes service
account token for a short-lived provider token (Anthropic, OpenAI, Google
Cloud, Azure, or any RFC 7523 / RFC 8693 endpoint) and adds it to the app's
HTTPS requests. SPIFFE identities from SPIRE are planned as an option.

- `waterwheel-controller`: serves each sidecar its bindings and injects the
  sidecar.
- `waterwheel-proxy`: the per-pod CONNECT proxy that adds the credentials.
- `waterwheel-agent`: Waterwheel's own SPIFFE agent, a later identity source; not
  deployed by default.

Status: scaffold. See [docs/design.md](docs/design.md),
[docs/milestone-1.md](docs/milestone-1.md), and
[docs/oidc.md](docs/oidc.md) for publishing the signing keys.

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
[docs/milestone-1.md](docs/milestone-1.md).

`LIMA_VM= make dev` uses Docker and kind on the local machine instead of Lima.
Behind a TLS-intercepting proxy (like a Riptides workstation with MITM on),
pass its CA, so that the VM's Docker and the sidecars trust it:
`EXTRA_ROOTS_FILE="$HOME/Library/Application Support/riptides/mitm-ca.pem" make e2e`
(or `make dev`).
`kubectl` uses `.state/kubeconfig`.

## License

Apache-2.0.
