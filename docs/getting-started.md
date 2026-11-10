# Getting started

This takes a cluster from no Waterwheel to an app that calls Anthropic with no
API key. It starts from an app that already runs: `support-bot`, a Deployment
in the `support` namespace, running as the service account `support-bot` and
calling `api.anthropic.com`. Use your own names instead. The app's identity is
its service account token, the default; for SPIRE, see [the design](design.md).

You need Kubernetes 1.29 or newer, because the proxy runs as a native sidecar,
plus Helm and kubectl.

## 1. Install Waterwheel

```bash
helm install waterwheel oci://ghcr.io/riptideslabs/charts/waterwheel --version 0.1.0 \
  --namespace waterwheel-system --create-namespace
kubectl -n waterwheel-system rollout status deployment/waterwheel-controller
```

Pick the version from [the releases](https://github.com/riptideslabs/waterwheel/releases).

## 2. Let the provider trust the cluster

The provider checks the pod's token against the keys of the cluster's issuer.
Find the issuer:

```bash
kubectl get --raw /.well-known/openid-configuration | jq -r .issuer
```

On EKS, GKE and AKS that URL is public, and the provider can fetch the keys
from it. On other clusters, upload the keys or host them;
[docs/oidc.md](oidc.md) shows how.

In the Anthropic Console, add the issuer and a federation rule for a service
account. The rule matches:

- issuer: the URL above;
- subject: `system:serviceaccount:support:support-bot`;
- audience: `https://api.anthropic.com`.

Note the organization, workspace, service account (`svac_…`) and federation
rule (`fdrl_…`) IDs for the next step.

## 3. Bind the credential

A `CredentialSource` says how to get the token. A `CredentialBinding` gives it
to a service account's pods for the hosts listed. Save these as
`credentials.yaml`, with the IDs from the last step:

```yaml
apiVersion: core.riptides.io/v1alpha1
kind: CredentialSource
metadata: {name: anthropic, namespace: support}
spec:
  anthropic:
    organizationId: <organization ID>
    workspaceId: default
    serviceAccountId: svac_...
    federationRuleId: fdrl_...
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

```bash
kubectl apply -f credentials.yaml
kubectl -n support get credentialbindings   # Ready: True
```

OpenAI, Google Cloud, Microsoft Entra ID, RFC 7523 and RFC 8693 endpoints work
the same way, with their own fields under `spec`; see
[the API](../api/v1alpha1/credentialsource_types.go).

## 4. Turn on the sidecar

```bash
kubectl label namespace support waterwheel.riptides.io/inject=enabled
kubectl -n support rollout restart deployment/support-bot
```

Only new pods get the sidecar, hence the restart. A pod can opt out with the
annotation `waterwheel.riptides.io/inject: "false"`.

The app needs no changes: the injector sets `HTTPS_PROXY` and the CA bundle,
and the proxy replaces whatever API key the SDK sends. Set
`automountServiceAccountToken: false` on the pod if the app shouldn't get
Kubernetes' default token; the sidecar's own tokens don't depend on it.

## 5. Check it

```bash
kubectl -n support get pods         # 2/2: the app and waterwheel-proxy
kubectl -n support events
kubectl -n support logs deployment/support-bot -c waterwheel-proxy
```

Each pod gets a `CredentialIssued` event once its proxy has a token, or a
`TokenExchangeFailed` warning with the provider's answer. To see the token
the proxy presents:

```bash
kubectl -n support exec deployment/support-bot -c waterwheel-proxy -- \
  waterwheel-proxy token https://api.anthropic.com
```

Its `iss`, `sub` and `aud` must match the federation rule.

## Upgrading

Helm doesn't upgrade CRDs, so apply the new release's first:

```bash
helm show crds oci://ghcr.io/riptideslabs/charts/waterwheel --version <new> |
  kubectl apply --server-side -f -
helm upgrade waterwheel oci://ghcr.io/riptideslabs/charts/waterwheel --version <new> \
  --namespace waterwheel-system
```
