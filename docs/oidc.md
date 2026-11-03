# Publishing the issuer's keys

A provider such as Anthropic, AWS or GCP accepts a subject token only if it
can check the signature. With service account tokens, the cluster's API server
signs them, so the provider needs the public keys of the cluster's service
account issuer (the JWKS). Only public keys are published.

Which providers support which way of getting the keys:

| Provider       | Fetches from the issuer URL | Accepts an uploaded JWKS |
|----------------|-----------------------------|--------------------------|
| Anthropic      | yes                         | yes                      |
| GCP            | yes                         | yes                      |
| AWS IAM        | yes                         | no                       |
| Azure Entra    | yes                         | no                       |
| OpenAI         | not checked yet             | not checked yet          |

The issuer string is the token's `iss` claim, and providers compare it
exactly. Changing it means registering again with every provider.

## Managed clusters

EKS, GKE and AKS already publish each cluster's issuer, with its discovery
document and keys, at a public URL. Register that URL with the provider, and
there is nothing to host or rotate:

- **EKS:**
  `aws eks describe-cluster --name <c> --query cluster.identity.oidc.issuer`
- **GKE:**
  `https://container.googleapis.com/v1/projects/<project>/locations/<location>/clusters/<cluster>`
- **AKS:** turn on the OIDC issuer (`--enable-oidc-issuer`), then read
  `oidcIssuerProfile.issuerUrl`.

## kind and self-managed clusters

The issuer is the API server's `--service-account-issuer`. On kind it is
`https://kubernetes.default.svc.cluster.local`. The API server serves the
discovery document at `/.well-known/openid-configuration` and the keys at
`/openid/v1/jwks`, but usually not publicly.

### Inline JWKS

Upload the keys to the provider. This is the simplest way, and the one the
milestone 1 live test uses:

```bash
kubectl get --raw /openid/v1/jwks > jwks.json
```

Register the issuer string with that JWKS. The keys change only when you
replace the API server's `--service-account-signing-key-file`, and on kind
never, so the upload keeps working. It needs a provider that accepts uploads.

### Bucket

Host copies of both documents at a public URL, and make that URL the issuer.
This is the usual self-hosted IRSA setup, and it works with every provider:

1. Set the API server's `--service-account-issuer` to the bucket's URL, for
   example `https://example-oidc.s3.eu-central-1.amazonaws.com`. Set
   `--service-account-jwks-uri` to `<issuer>/openid/v1/jwks`. The API server
   accepts the flag more than once, and signs new tokens with the first one.
   Keep the old issuer listed after it, so existing tokens stay valid while
   the pods' tokens rotate.
2. Copy the documents into the bucket, with public read on just these two
   objects:

   ```bash
   kubectl get --raw /.well-known/openid-configuration > openid-configuration
   kubectl get --raw /openid/v1/jwks > jwks
   ```

   Upload them as `.well-known/openid-configuration` and `openid/v1/jwks`.
3. Copy the keys again after rotating the signing key. Publish the new key
   before the API server starts signing with it.

Exposing the API server's own discovery endpoints to the internet also works,
by binding `system:service-account-issuer-discovery` to
`system:unauthenticated`. It puts the API server on the internet, though, so
the bucket is better.

## With SPIRE

With the `spire` identity source, the subject tokens are JWT-SVIDs, so SPIRE's
keys are the ones to publish. `global.spire.jwtIssuer` in SPIFFE's `spire`
chart sets the issuer; it defaults to `https://oidc-discovery.<trust domain>`.
A provider's rule then matches `sub = spiffe://<trust domain>/<workloadID>`.
The e2e's fake provider reads the keys from the OIDC discovery provider inside
the cluster, over plain HTTP. For a real provider, there are three ways:

- **Ingress.** Expose SPIRE's OIDC discovery provider, which the chart deploys
  by default, with
  `spiffe-oidc-discovery-provider.ingress.{enabled,className,host,tlsSecret}`.
  The certificate must be publicly trusted.
- **Bucket.** SPIRE's bundle publisher writes the JWKS whenever the keys
  change. Set `spire-server.bundlePublisher.awsS3`, or `gcpCloudStorage`, with
  `format: jwks`, and upload a static discovery document next to it once.
- **Inline.** Export the JWT keys and upload them:

  ```bash
  kubectl -n spire-server exec spire-server-0 -c spire-server -- \
    /opt/spire/bin/spire-server bundle show -format spiffe |
    jq '{keys: [.keys[] | select(.use == "jwt-svid") | del(.use) + {use: "sig",
      alg: (if .kty == "EC" then {"P-256": "ES256", "P-384": "ES384"}[.crv] else "RS256" end)}]}'
  ```

  SPIRE rotates its keys, though. It publishes the next key at
  `notAfter − min(TTL/2, 30d)` and starts signing with it at
  `notAfter − min(TTL/6, 7d)`. With the chart's default 24-hour `caTTL`, that
  leaves hours 12 to 20 for the next upload. Inline therefore needs a long
  `spire-server.caTTL`: the KCD demo used 90 days, which gives a window from
  day 60 to day 83.
