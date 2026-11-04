# Waterwheel design

Status: draft. Reviewed 2026-10-30. On 2026-11-02, Kubernetes service account
tokens became the first identity source, and on 2026-11-03 SPIRE the second.
The decisions are recorded at the end of this document.

Waterwheel turns a pod's identity into the API credentials it needs, so no pod
holds a long-lived key. It is the KCD Budapest demo
(`riptideslabs/kcd-budapest-demo`) made into a product.

For plain credential injection, the pod's Kubernetes service account is
identity enough. Kubernetes signs a short-lived token for it, and the provider
trusts the cluster's issuer. This is how AWS IRSA and GKE and AKS workload
identity work. SPIFFE identities from SPIRE, and later from Waterwheel's own
agent, are options for finer-grained identities.

## Components

```
 pod ─────────────────────────────────────────┐
 │ app ── HTTPS_PROXY ──▶ waterwheel-proxy ───────┼── token (aud waterwheel-controller) ──▶ waterwheel-controller
 │                          ▲                 │                                       bindings API, TokenReview
 │        projected SA tokens, sidecar only   │                                       webhook: inject the sidecar
 └──────────────────────────┼─────────────────┘
              kubelet ──────┘  writes and rotates them; kube-apiserver signs them

 waterwheel-proxy ── SA token → access token (tokenex) ──▶ provider STS ── checks the cluster's JWKS
```

- **Kubernetes provides the identity.** The API server signs service account
  tokens, and the kubelet projects them into the sidecar and rotates them.
  There is no agent and no extra infrastructure.
- **waterwheel-controller** serves each proxy its bindings, injects the sidecar and
  reports status on Waterwheel's objects.
- **waterwheel-proxy** is the per-pod sidecar. It runs a CONNECT proxy that
  intercepts TLS only for bound destinations and gets each provider's token
  with tokenex.

All of this is in one Go module, `go.riptides.io/waterwheel`, under Apache-2.0.

## Identity sources

| Source | Subject token | The provider sees | Needs |
|---|---|---|---|
| `serviceAccountToken` (default, M1) | projected service account token | `system:serviceaccount:<ns>:<sa>` | the cluster's issuer published (see [oidc.md](oidc.md)) |
| `spire` | JWT-SVID | `spiffe://<td>/<workloadID>` | SPIRE, with its controller manager and CSI driver |
| `waterwheel` (later) | JWT-SVID from Waterwheel's agent | `spiffe://<td>/<workloadID>` | Waterwheel's agent and signer |

The chart's `identity.source` picks one. Everything else (the bindings API,
the injector, the proxy and the API) is the same for all three.

## Flows

**Admission.** The mutating webhook handles each new pod in a namespace with
injection turned on. It works out the pod's bindings: those whose `workloadID`
is `ns/<ns>/sa/<pod's service account>`. From their sources it collects the
audiences. It then adds the sidecar, with one projected token per audience plus
one with audience `waterwheel-controller`. The tokens are valid for an hour, and
the kubelet replaces them before they expire.

Audiences are fixed when the pod is created. A new binding whose audience the
pod already has applies right away; one that needs a new audience applies
after the pod restarts. The webhook records each pod's audiences in its
`waterwheel.riptides.io/audiences` annotation. A binding's `PodsUpToDate` condition
names the pods that need a restart. The alternative is to
request tokens at runtime, which needs permission to create service account
tokens, and that is too powerful to give the controller.

With SPIRE, none of this applies. The sidecar gets the Workload API, plus the
one `waterwheel-controller` token, and asks for a JWT-SVID per audience at
runtime.

**Bindings.**

- The proxy calls the controller's bindings API over TLS. It trusts the
  controller's serving certificate through a CA the injector mounts.
- It authenticates with its `waterwheel-controller` token. A TokenReview gives the
  controller the service account, the pod's name and UID.
- The controller checks that the pod exists and runs as that service account,
  then returns the bindings for `ns/<ns>/sa/<sa>`: destinations, the provider
  config, the subject token's audience and the injection header. With SPIRE,
  it first works out the pod's WorkloadIdentity (see [SPIRE](#spire)), and
  returns the bindings of its workload ID, with the SPIFFE ID to ask SPIRE for.
- It is `GET /v1/bindings` over HTTPS. With `?watch=true` it streams one JSON
  snapshot per line, and a new one whenever a binding or source changes, like
  a Kubernetes watch. A stream ends after 10 minutes, and the proxy reconnects
  with its current token, so a deleted pod loses access.

The same API serves proxies with every identity source, so the controller
never needs a SPIFFE identity of its own.

**Credentials.** The proxy runs one tokenex loop per binding (the demo's
`creds.go`). The subject token is the projected token for the source's
audience, read from its file on every exchange, since the kubelet rotates it.
tokenex has no file-based identity token provider yet. A small one lives in
Waterwheel first, and moves to tokenex later. With SPIRE, the subject token is
a JWT-SVID for that audience and the pod's SPIFFE ID, fetched from the
Workload API on every exchange with tokenex's `spiffe` provider. Exchanged
tokens stay in the proxy's memory.

**Proxy.** For each `CONNECT host:port`:

- If a binding matches, the proxy terminates TLS with a leaf certificate from
  its per-pod CA. It then sets the binding's header (by default
  `Authorization: Bearer <token>`), strips the provider's key headers
  (`X-Api-Key` for Anthropic) and forwards the request.
- Otherwise it passes the connection through as a plain TCP tunnel and logs it.
- Only HTTP/1.1 is intercepted at first, WebSocket upgrades included. The
  proxy offers only `http/1.1` to the app, so gRPC to a bound host fails.

The per-pod CA's key lives in a memory-backed volume mounted only into the
sidecar. A sidecar restart therefore keeps the same CA, which matters because
apps load the CA bundle once. The key disappears with the pod.

## API

Waterwheel has its own API, `core.riptides.io/v1alpha1`, in `api/v1alpha1`. It is
made of Go types with kubebuilder markers, and controller-gen generates the
CRDs and deep copies from them. It doesn't import or copy the platform's API
module. It is modeled on that API, though, and keeps its group, kind names and
field names wherever the two overlap, so they can be merged later.

- **CredentialBinding** has `workloadID`, `credentialSource` and
  `propagation.injection`. With service account tokens, the `workloadID` is
  `ns/<ns>/sa/<sa>`, and pods running as that service account get the binding.
  - `injection.destinations` lists `{host, port}` pairs. The host is exact or
    `*.example.com`, and the port defaults to 443.
  - The optional `header` and `scheme` default to `Authorization` and `Bearer`.
  - There is no `Service` kind. A CONNECT proxy already sees the hostname.
- **CredentialSource** has exactly one of `anthropic`, `openai`, `gcp`,
  `azure`, `rfc7523` or `rfc8693`. Fields are spelled as in the platform
  (`organizationId`, `oidcProviderId`, `idTokenAudience`, …).
  - The audience fields (`idTokenAudience` or `audience`) set the subject
    token's audience. With service account tokens, that becomes a projected
    token audience.
  - `gcp` exchanges the subject token at Google's STS through a workload
    identity pool provider, and can then impersonate a service account. Its
    audience defaults to `https:` followed by the provider's resource name.
  - `azure` presents the subject token as the client assertion of an Entra ID
    app or managed identity, and gets a token for one scope. Its audience
    defaults to `api://AzureADTokenExchange`.
  - `rfc8693` requests go out as JSON, because that's how tokenex sends them
    (it was written against OpenAI's endpoint). RFC 8693 says form encoding,
    so a standards-following STS needs a body-format option in tokenex, like
    the one its RFC 7523 client has.
  - There are no `idTokenClaims` or `idTokenLifetime`, because the subject
    token's claims and lifetime aren't Waterwheel's to set.
  - `aws` comes later. tokenex returns SigV4 keys rather than a token, so the
    proxy would have to re-sign each request.
  - Secret-backed sources come in M2.
- **WorkloadIdentity** is only used with SPIFFE sources. It has `workloadID`,
  `selectors` and `svid`, and no `scope`.
  - Selector entries are ORed, and the keys inside an entry are ANDed.
  - With SPIRE, only the pod keys apply: `k8s:pod:*` and `k8s:label:*`. The
    sidecar fetches the SVIDs, so container and process keys would describe
    the sidecar. They, `process:name` and `process:binary:path` return with
    Waterwheel's own agent.

**Validation happens in the API server, not in a webhook:**

- CEL rules in the CRDs check that a source names exactly one provider, that
  selector keys are known, and the host syntax and TTL bounds.
- A ValidatingAdmissionPolicy enforces the namespace rules below.

This holds even while the controller is down, and `kubectl apply -f dir/`
doesn't depend on file order. Cross-object problems, such as a binding whose
source doesn't exist, show up as status conditions rather than rejections.

**Namespaces are the trust boundary:**

- A `CredentialBinding` or `WorkloadIdentity` in namespace `N` must have a
  `workloadID` starting with `ns/N/`.
- A `CredentialBinding` in `N` uses a `CredentialSource` from `N`.
- Objects in `waterwheel-system` may use any ID. Only cluster admins should be able
  to write there.
- The `waterwheel/` prefix is reserved for Waterwheel's own identities.

The policy enforces these rules at admission. The controller enforces them
again when it serves bindings, because it only returns bindings that match the
pod's own namespace and service account. Even if someone deletes the policy,
one namespace still can't take another namespace's credentials.

The chart ships aggregated ClusterRoles, so namespace `admin` and `edit` can
manage the three kinds and `view` can read them. Anyone who can create pods
with a service account gets that service account's bindings. This is the same
trust model as IRSA.

The demo as CRs, with service account tokens:

```yaml
apiVersion: core.riptides.io/v1alpha1
kind: CredentialSource
metadata: {name: anthropic, namespace: support}
spec:
  anthropic:
    organizationId: …
    workspaceId: default
    serviceAccountId: svac_…
    federationRuleId: fdrl_…
    idTokenAudience: ["https://api.anthropic.com"]
---
apiVersion: core.riptides.io/v1alpha1
kind: CredentialBinding
metadata: {name: support-bot-anthropic, namespace: support}
spec:
  workloadID: ns/support/sa/support-bot
  credentialSource: anthropic
  propagation:
    injection:
      destinations:
      - host: api.anthropic.com
```

Anthropic's federation rule then matches
`sub = system:serviceaccount:support:support-bot`, with the cluster's service
account issuer.

## Publishing the issuer's keys

Providers check the subject token against the cluster's service account
issuer keys. EKS, GKE and AKS publish their clusters' issuers publicly, so
there is nothing to do there.

On kind and self-managed clusters, there are two ways, described in
[oidc.md](oidc.md):

- upload the API server's `/openid/v1/jwks` inline to the provider, or
- copy the discovery documents to a bucket.

Those keys rarely rotate (on kind, never), so either way keeps working.

## Sidecar injection

The webhook handles Pod CREATE in namespaces labeled
`waterwheel.riptides.io/inject=enabled`. A pod can opt out with the annotation
`waterwheel.riptides.io/inject: "false"`. It is the demo injector with six
changes:

- projected token volumes instead of the SPIRE socket, mounted only into the
  sidecar. There is one file per audience, named `aud-` plus a hash of the
  audience; `waterwheel-proxy token <audience>` prints a token's claims. With
  SPIRE, the socket comes back, from a CSI volume;
- the CA key volume;
- the CA bundle mounts and env vars below. A pod annotated
  `waterwheel.riptides.io/ca-bundle: env-only` gets the env vars without the
  mounts;
- the controller's CA, passed to the sidecar in `WATERWHEEL_CONTROLLER_CA`, for the
  bindings API;
- `automountServiceAccountToken` is left alone;
- `failurePolicy: Fail`.

The webhook's serving certificate is self-managed, like the demo injector's,
but kept in a Secret so every controller replica serves the same chain. The
bindings API uses the same certificate.

**CA trust, with no manual steps.** When the sidecar starts, it writes a
combined bundle: the Mozilla roots (embedded from `x509roots/fallback/bundle`)
plus the pod CA, plus an optional cluster-wide `waterwheel-extra-roots` ConfigMap.

The ConfigMap is set with the chart's `extraRoots` value. It's for a corporate
TLS-intercepting proxy between the cluster and the internet, or for a private
provider. The proxy trusts the same CAs for its own upstream connections: it
points `SSL_CERT_FILE` at the public roots plus those CAs, which covers
tokenex and the cloud SDKs too.

The injector mounts the combined bundle with `subPath` over the common OS
bundle paths:

- Debian, Ubuntu and Alpine: `/etc/ssl/certs/ca-certificates.crt`
- Alpine: `/etc/ssl/cert.pem`
- RHEL: `/etc/pki/tls/certs/ca-bundle.crt` and
  `/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem`
- SUSE: `/etc/ssl/ca-bundle.pem`

It also sets these env vars to the same file:

- `SSL_CERT_FILE`
- `CURL_CA_BUNDLE`
- `REQUESTS_CA_BUNDLE`
- `NODE_EXTRA_CA_CERTS`
- `AWS_CA_BUNDLE`
- `GRPC_DEFAULT_SSL_ROOTS_FILE_PATH`

The bundle has to be the combined one. Anthropic's Python SDK, for example,
*replaces* its trust store with `SSL_CERT_FILE`, so a bundle with only the pod
CA would break every connection the proxy passes through. Roots that the
Mozilla store distrusts after a date are left out, because a PEM bundle can't
express that date. Java comes later.

## Not in v0

- Transparent interception with iptables, for clients that ignore
  `HTTPS_PROXY`.
- HTTP/2 interception.
- Java trust stores.
- Secret-backed sources (M2).
- Federation.
- Waterwheel's own agent and signer (below).

## SPIFFE identity sources

Both are selected with `identity.source`, and both use the same bindings API,
injector, proxy and API. They add `WorkloadIdentity`, whose selectors can tell
apart pods that share a service account. They also give SPIFFE IDs, which can
span clusters in one trust domain.

### SPIRE

The `spire` source, since 2026-11-03. SPIRE comes from SPIFFE's Helm charts
(`spiffe/helm-charts-hardened`; chart 0.30.4 ships SPIRE 1.15.5), or an
existing install is used. It needs SPIRE's controller manager and CSI driver.
`hack/spire.sh` installs it on kind.

- **Which identity a pod gets.** The controller decides, and tells both SPIRE
  and the proxy, so the two agree (`internal/identity`):
  - A pod gets the workload ID of the WorkloadIdentity whose selector entry
    matches it. Only identities in the pod's namespace and in
    `waterwheel-system` apply. One outside `waterwheel-system` only selects
    pods in its own namespace.
  - When entries of several identities match, the most specific wins: the
    entry that has every key and value of the others, and more. A
    namespace-wide identity can have exceptions that way. The general
    identity's `Shadowed` condition names the specific ones.
  - When no entry is the most specific, the pod gets no identity, so no
    bindings. The proxy logs why.
  - Two identities with the same selector entry and different workload IDs
    would always tie. The newer one isn't put into effect: its `Ready`
    condition is False, with reason `SelectorsOverlap`. The platform's webhook
    rejects such an identity at admission. Waterwheel checks nothing across
    objects at admission, so here it is status.
- **Registration.** For each selector entry of an identity in effect, the
  controller writes one ClusterSPIFFEID, named
  `waterwheel.<namespace>.<name>.<entry>`:
  - its `spiffeIDTemplate` is `spiffe://{{ .TrustDomain }}/<workloadID>`;
  - its namespace selector is the identity's namespace, and the label keys
    become its pod selector;
  - its workload selectors are the service account and pod name keys, plus
    `k8s:container-name:waterwheel-proxy`, so only the sidecar gets the SVID;
  - its `className` is the SPIRE install's (chart value `spire.className`);
  - it is labeled `app.kubernetes.io/managed-by: waterwheel-controller`.
    ClusterSPIFFEIDs are cluster-scoped, so a WorkloadIdentity can't own them.
    Rather than keep finalizers, the controller deletes the labeled ones it no
    longer wants. It syncs after every change and every five minutes.
- **Turn off SPIRE's default ClusterSPIFFEID.** It gives every pod
  `spiffe://<td>/ns/<ns>/sa/<sa>`, which would bypass the selectors.
  `hack/spire.sh` turns it off.
- **Selectors map like this:**

| Waterwheel | ClusterSPIFFEID |
|---|---|
| `k8s:pod:namespace` | `namespaceSelector` on `kubernetes.io/metadata.name` |
| `k8s:label:<k>` | `podSelector.matchLabels` |
| `k8s:pod:serviceaccount` | `k8s:sa` |
| `k8s:pod:name` | `k8s:pod-name` |

  `k8s:container:*` and `process:*` would select the sidecar, which fetches
  the SVIDs, so an identity with them isn't put into effect (reason
  `UnsupportedSelector`). They return with Waterwheel's own agent.
- **The socket.** The injector mounts SPIRE's agent socket into the sidecar
  only, from a `csi.spiffe.io` volume. That works in Pod Security `baseline`
  namespaces. The app container gets no socket.
- **Startup.** SPIRE issues a new pod's SVIDs several seconds after it
  starts; 7 to 10 on kind. The proxy waits for them before it listens, within
  the 25 seconds it allows itself for startup, so the app's first requests
  already have an identity.
- **Bindings.** A binding's `Ready` condition is False, with reason
  `NoWorkloadIdentity`, until an identity in effect gives its workload ID.
  Audiences aren't fixed, so there's no `PodsUpToDate`.
- **Keys.** SPIRE rotates its keys, with the next key published at
  `notAfter − min(TTL/2, 30d)` and signing from `notAfter − min(TTL/6, 7d)`.
  Its OIDC discovery provider or its bundle publisher (S3 or GCS, `jwks`
  format) makes the keys available to providers. An inline upload needs a long
  `caTTL`: the chart default is 24h, and the demo used 90 days.

The components with SPIRE, next to the diagram under [Components](#components):

```
 pod ─────────────────────────────────────────────┐
 │ app ── HTTPS_PROXY ──▶ waterwheel-proxy ───────┼── token (aud waterwheel-controller) ──▶ waterwheel-controller
 │                          ▲                     │                                          bindings, SPIFFE ID
 │       Workload API (CSI socket), sidecar only  │                                                   │
 └──────────────────────────┼─────────────────────┘                                                   │ ClusterSPIFFEIDs
                            │                                                                         ▼
          spire-agent ──────┘ attests the sidecar ◀── entries, SVIDs ────────────────────────── spire-server
                              issues JWT-SVIDs

 waterwheel-proxy ── JWT-SVID → access token (tokenex) ──▶ provider STS ── checks SPIRE's JWKS
```

### Waterwheel's own agent and signer (later)

This is the original plan:

- **waterwheel-agent.** A DaemonSet with `hostPID` serves the Workload API. It
  attests each caller in four steps:
  - it takes the peer PID from `SO_PEERCRED` and pins the process with a pidfd;
  - it reads `/proc/<pid>/cgroup` to get the container;
  - it asks the kubelet's `/pods` for the pod, through the fine-grained
    `nodes/pods` permission, checked on kind on 2026-10-30;
  - it re-checks the pidfd.

  It adds `process:name` and `process:binary:path` selectors.
- **Agent bootstrap.** Agents bootstrap with a projected token and a
  TokenReview, and receive `spiffe://<td>/waterwheel/agent/<node>`.
- **Signing.** The controller signs SVIDs only for pods on the requesting
  agent's node.
- **Keys.** The keys live in an `waterwheel-keys` Secret, and rotate on SPIRE's
  schedule. The controller serves OIDC discovery, behind the chart's optional
  OIDC Ingress.

## Decisions

- **2026-10-30, Q5/Q6 – API.** Waterwheel has its own API rather than a synced copy
  of the platform's. `WorkloadIdentity` has no `scope`. The two APIs may be
  merged later.
- **Q7 – No `Service` kind.** Destinations are listed in the binding.
- **Q8 – Namespace rule.** Applied as above, and enforced at admission and by
  the controller.
- **Q9 – JWKS publication.** See [oidc.md](oidc.md).
- **Q10 – Secret-backed sources** come in M2.
- **2026-11-02 – Service account tokens first.** Plain credential injection
  needs no SPIFFE: the pod's projected service account token is the subject
  token. SPIRE, and then Waterwheel's own agent, come later as identity sources
  behind the same API. Bindings come from the controller, authenticated by
  TokenReview, for every source.
- **2026-11-03 – SPIRE source.** The controller works out each pod's
  WorkloadIdentity, writes the ClusterSPIFFEIDs from the same result, and
  names the SPIFFE ID in the bindings API, so SPIRE and the proxy agree. When
  several identities match a pod, the most specific entry wins. Identities
  with the same selectors are the platform's admission rule, applied as
  status: the newer one isn't put into effect. Only pod selectors apply,
  because the sidecar is the SVID's caller.
- **2026-11-02 – Repository.** The code is planned to move into the tokenex
  repo as its root module, under the tokenex name and Apache-2.0. The library
  moves to its own module and keeps its MIT license. The timing is open.

## Open

1. **Token exchange in the sidecar,** with the controller serving the pod's
   bindings. *Recommended.* Tokens never leave the pod, and this is how the
   demo works. The cost is one exchange per pod rather than one per identity.
2. **CA trust through a combined bundle at the OS paths plus env vars.**
   *Recommended.* The mount hides private CAs baked into the app image. Those
   go into `waterwheel-extra-roots`, and a per-pod annotation can switch to
   env-only.
3. **Unbound destinations pass through,** with a log line per tunnel.
   *Recommended.* A strict per-namespace mode can come later.
4. **Open-core positioning, with an API the platform can adopt later.**
   *Recommended.*
