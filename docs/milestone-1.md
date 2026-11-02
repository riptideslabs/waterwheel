# Milestone 1: the KCD demo on Waterwheel

Goal: `make e2e` brings up a fresh kind cluster with Waterwheel and runs the demo's
five steps. In the demo, SPIRE and a ConfigMap did the work. Here the pod's
projected service account token is the identity, and Waterwheel's two CRDs, its
injector, its proxy and its bindings API do the rest. Every step checks its own
result.

Done when it passes twice in a row from scratch, both locally and in CI, and
the live Anthropic mode passes once by hand.

## Setup

- **Lima VM and kind cluster.** The VM is `waterwheel-dev`, made from the demo's
  Lima template (docker-rootful with the overlay2 store). The kind cluster is
  `waterwheel`, with its API server on `127.0.0.1:7543`. The demo's VM and
  cluster are left alone.
- **Scripts without Lima.** With `LIMA_VM=` empty, the scripts run Docker and
  kind directly, which is how CI uses them on an Ubuntu runner.
- **The issuer.** kind's service account issuer is
  `https://kubernetes.default.svc.cluster.local`. No provider can reach it,
  which is fine: the fake provider reads the keys inside the cluster, and the
  live mode uploads them.
- **Images.** Binaries are cross-compiled on the host, wrapped in scratch
  images and loaded with `kind load`, as in the demo.
- **Install and reset.** `make deploy` installs Waterwheel's CRDs and chart.
  `make reset` recreates the `support` namespace:
  - the curl Deployment;
  - the `CredentialSource` and `CredentialBinding`.

## The five steps

These are `test/e2e/steps/N-*.sh`, written in the demo's style (the same
`run` and `title` helpers and `PAUSE=1`) plus an `expect` helper.

1. **No key.** The app container has no Secrets, no credential-looking env
   vars and no `/var/run/secrets`. Waterwheel is running in `waterwheel-system`.
2. **The curl returns 401.**
3. **Turn the proxy on.** `kubectl label namespace support
   waterwheel.riptides.io/inject=enabled`, then a rollout restart. Expect the pod at
   2/2 with an `waterwheel-proxy` init container that has `restartPolicy: Always`
   and two projected tokens, with audiences `https://api.anthropic.com` and
   `waterwheel-controller`.
4. **The same curl returns 200.** `waterwheel-proxy token` prints the decoded
   subject token:
   - `sub` = `system:serviceaccount:support:support-bot`;
   - `aud` = `https://api.anthropic.com`;
   - `iss` = the cluster's issuer.

   The proxy log shows bindings → subject token → access token → request.
5. **`exfil.sh` prints "Nothing to steal."** The app container has no token
   files, because the projected tokens are mounted only into the sidecar. The
   mount check finds only the CA bundle.

## Provider modes

**`E2E_PROVIDER=fake`** is the default and is what CI runs. It is offline.
`test/e2e/fakeprovider` is a test-only image with two hosts:

- `sts.fake.waterwheel.test/token` is an RFC 8693 endpoint. It checks the subject
  token against the API server's `/openid/v1/jwks`, which it reads with its own
  service account token. It checks `iss`, `aud`, `exp` and an allowed `sub`,
  and returns an opaque token.
- `api.fake.waterwheel.test/v1/messages` returns Anthropic-shaped answers: 401
  without a valid token and 200 with one.

Three pieces of setup make this work:

- A CoreDNS rewrite sends `*.fake.waterwheel.test` to the fake provider.
- The fake provider's certificate comes from an e2e CA. That CA is baked into a
  test copy of the curl image, which makes step 2 a 401 rather than a TLS
  error. It is also in `waterwheel-extra-roots`, so it survives the bundle mount
  in step 4.
- The proxy trusts it upstream through the same ConfigMap.

**`E2E_PROVIDER=anthropic`** is run by hand. It reads the Anthropic IDs from a
gitignored `providers.env`. It needs a new issuer and federation rule in the
Anthropic Console, which waits for Nándor's OK:
- the issuer is the cluster's issuer string, with the API server's
  `/openid/v1/jwks` uploaded inline;
- the rule matches `sub = system:serviceaccount:support:support-bot` and
  `aud = https://api.anthropic.com`.

kind's signing key never rotates, so the upload stays valid as long as the
cluster lives.

## Work items, in order

1. **Scaffold.** Done.
2. **API checks on kind.** Done: `make api-check` runs the samples in
   `test/api` against the cluster.
3. **Controller: bindings API.** Done on 2026-11-02:
   - `GET /v1/bindings`, and a watch that streams JSON lines, over HTTPS with
     a CA kept in the `waterwheel-tls` Secret;
   - authenticated by TokenReview, checking the pod;
   - the namespace rules enforced again;
   - each binding's `Ready` condition (`SourceNotFound`, `ForeignWorkloadID`).

   Checked on kind with real projected tokens: the right bindings for the right
   service account, 401 or 403 otherwise, and the watch picking up changes.
4. **Injection webhook.** Done on 2026-11-02. It is the demo injector, moved
   into the controller, with these additions:
   - one projected token per audience the pod's bindings need, plus one for
     `waterwheel-controller`;
   - the CA key volume;
   - the CA bundle mounts;
   - the controller's CA for the bindings API;
   - the serving certificate from the `waterwheel-tls` Secret, whose CA the
     controller writes into the webhook configuration.

   The binding status says when a pod needs a restart. The proxy's start came
   along, because the app can only mount the bundle once the sidecar has
   written it: the per-pod CA with its key kept across restarts, and the
   combined bundle.

   Checked on kind with the demo's curl Deployment:
   - the pod comes up 2/2;
   - the app has the env vars and the bundle at every OS path, including
     Alpine's symlinked `cert.pem`, but no tokens and no `/var/run/secrets`;
   - the sidecar's tokens carry the right `sub` and `aud`;
   - a crashed sidecar comes back with the same CA, and the app doesn't
     restart;
   - a binding with a new audience reports `RestartRequired` until the
     rollout;
   - opted-out pods and unlabeled namespaces aren't touched.
5. **Proxy.** Done on 2026-11-02:
   - the demo's `mitm.go` and `creds.go`, ported;
   - a bindings watch client;
   - a token-file identity provider for tokenex, with no tokenex change;
   - routes from the bindings, with exact and `*.` hosts, and tunnels for
     everything else;
   - a mapping from all six CredentialSource providers to their tokenex
     calls;
   - `waterwheel-extra-roots`, trusted upstream and added to the app's bundle.

   Checked on kind behind the workstation's TLS-intercepting proxy, with its
   CA as an extra root:
   - unbound `example.com` passes through with 200;
   - `api.anthropic.com` is intercepted with the pod CA;
   - the exchange reaches Anthropic's real token endpoint, which refused the
     sample IDs with a 400, and the app got a 403 `permission_error` with
     Anthropic's reason.

   Unit tests run the proxy end to end against TLS servers: token added and
   the key stripped, pass-through untouched, and 403 without a token.
6. **Helm chart.** Mostly done along the way: RBAC for TokenReviews, pods
   and the TLS Secret, the `extraRoots` value, and `make deploy` (which
   restarts the controller, since dev images keep their tag). PodSecurity
   labels remain.
7. **e2e.** Done on 2026-11-02:
   - `test/e2e/fakeprovider`, which accepts JSON as well as form bodies,
     because tokenex sends RFC 8693 requests as JSON;
   - the e2e CA, with a Go cert generator;
   - steps 1–5, with checks;
   - `make e2e` and `make demo`;
   - `.github/workflows/e2e.yml`.

   `make e2e` passed twice in a row from a fresh cluster, in 55s and 52s,
   behind the workstation's TLS-intercepting proxy with `EXTRA_ROOTS_FILE`.
   The CI job hasn't run yet, because the repo isn't on GitHub.
8. **Live run against Anthropic,** once. This waits for an issuer and
   federation rule in the Anthropic Console, which needs Nándor's OK.

Not in M1:
- the SPIRE source and Waterwheel's own agent;
- multiple controller replicas;
- Secret-backed sources.

## Risks

- **Audiences are fixed at admission.** A binding that needs a new audience
  only reaches existing pods after a restart. Its status has to say so
  clearly, or this will look like a bug.
- **Startup order.** The sidecar's first exchange happens before the app
  starts. The proxy reports ready once it has its bindings, and the app starts
  after the sidecar, so the app never starts without its bindings.
- **Some images won't take the bundle mount.** A `subPath` file mount over a
  symlinked bundle (Alpine's `/etc/ssl/cert.pem`, RHEL's `ca-bundle.crt`) has
  to be tested on each base image. M1 covers curl (Alpine). Debian, UBI and
  distroless go into an e2e matrix in M2.
