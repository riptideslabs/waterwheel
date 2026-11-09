# Releasing

Pushing a `v*` tag runs [release.yml](../.github/workflows/release.yml). It
refuses a tag whose commit isn't on main, runs CI and the e2e tests on the
commit, then:

- builds `waterwheel-controller`, `waterwheel-proxy` and `waterwheel-agent` for
  linux/amd64 and linux/arm64, with SBOM and provenance attestations, as
  `ghcr.io/riptideslabs/<name>:<tag>`;
- packages the chart as `oci://ghcr.io/riptideslabs/charts/waterwheel`, with
  the tag as its version and appVersion (`v0.1.0` makes chart `0.1.0`, whose
  images are `v0.1.0`);
- signs the images and the chart with cosign, keyless;
- makes a GitHub release with generated notes and the chart attached. A tag
  with a suffix, like `v0.1.0-rc.1`, makes a pre-release.

To release main:

```bash
git fetch origin
git tag -a v0.1.0 -m v0.1.0 origin/main
git push origin v0.1.0
```

The chart in the repository keeps `appVersion: dev`, for images built locally
(`make deploy`). Binaries log the version they were built from.

## Verifying a release

```bash
cosign verify ghcr.io/riptideslabs/waterwheel-proxy:v0.1.0 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity https://github.com/riptideslabs/waterwheel/.github/workflows/release.yml@refs/tags/v0.1.0
```

The identity names the tag, so a signature made for another tag doesn't
verify. The chart is verified the same way, as
`ghcr.io/riptideslabs/charts/waterwheel:0.1.0`.
