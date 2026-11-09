// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"

	"go.riptides.io/tokenex/pkg/azure"
	"go.riptides.io/tokenex/pkg/credential"
	"go.riptides.io/tokenex/pkg/gcp"
	"go.riptides.io/tokenex/pkg/option"
	"go.riptides.io/tokenex/pkg/rfc7523"
	"go.riptides.io/tokenex/pkg/rfc8693"
	"go.riptides.io/tokenex/pkg/token"
	"go.riptides.io/tokenex/pkg/token/spiffe"
	"go.riptides.io/waterwheel/internal/bindings"
)

var (
	errNotJWT     = errors.New("not a JWT")
	errNoProvider = errors.New("the source names no provider")
	errNoAudience = errors.New("the pod started before a binding needed this audience; restart it")
)

// Subjects returns a binding's subject token provider, for the pod's SPIFFE
// ID (empty with service account tokens).
type Subjects func(spiffeID string, b bindings.Binding) (token.IdentityTokenProvider, error)

// TokenFiles gives each binding the pod's projected service account token for
// its audience, from dir.
func TokenFiles(dir string) Subjects {
	return func(_ string, b bindings.Binding) (token.IdentityTokenProvider, error) {
		// Projected tokens are fixed when the pod starts, so a missing one stays missing.
		if _, err := os.Stat(filepath.Join(dir, bindings.TokenFile(b.Audience))); errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("no token for audience %s: %w", b.Audience, errNoAudience)
		}

		return TokenFile(dir, b.Audience), nil
	}
}

// SPIRE gives each binding a JWT-SVID for the pod's SPIFFE ID and the
// binding's audience, fetched from the Workload API on every exchange.
func SPIRE(source jwtsvid.Source) Subjects {
	return func(spiffeID string, b bindings.Binding) (token.IdentityTokenProvider, error) {
		id, err := spiffeid.FromString(spiffeID)
		if err != nil {
			return nil, fmt.Errorf("SPIFFE ID %q: %w", spiffeID, err)
		}

		return spiffe.NewIdentityTokenProvider(spiffe.WithAudience(b.Audience), spiffe.WithSPIFFEID(id), spiffe.WithSource(source))
	}
}

// TokenFile is the subject token provider for an audience: the pod's projected
// service account token, read on every call because the kubelet rotates it.
func TokenFile(dir, audience string) token.IdentityTokenProvider {
	path := filepath.Join(dir, bindings.TokenFile(audience))

	return token.IdentityTokenProviderFunc(func(context.Context, ...option.Option) (credential.Token, error) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return credential.Token{}, err
		}

		tok := strings.TrimSpace(string(raw))

		claims, err := Claims(tok)
		if err != nil {
			return credential.Token{}, err
		}

		return credential.Token{Token: tok, ExpiresAt: time.Unix(claims.Exp, 0)}, nil
	})
}

// TokenClaims are the claims of a subject token the proxy logs.
type TokenClaims struct {
	Iss string `json:"iss"`
	Sub string `json:"sub"`
	Aud any    `json:"aud"`
	Exp int64  `json:"exp"`
}

// Claims decodes a JWT's claims without checking its signature; the provider
// does that.
func Claims(jwt string) (TokenClaims, error) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return TokenClaims{}, errNotJWT
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return TokenClaims{}, fmt.Errorf("%w: %w", errNotJWT, err)
	}

	var c TokenClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		return TokenClaims{}, fmt.Errorf("%w: %w", errNotJWT, err)
	}

	return c, nil
}

// ProviderName is a binding's provider, for logs.
func ProviderName(b bindings.Binding) string {
	p := b.Provider

	switch {
	case p.Anthropic != nil:
		return "Anthropic"
	case p.OpenAI != nil:
		return "OpenAI"
	case p.GCP != nil:
		return "Google Cloud"
	case p.Azure != nil:
		return "Microsoft Entra ID"
	case p.RFC7523 != nil:
		return "RFC 7523 " + p.RFC7523.TokenEndpointURL
	case p.RFC8693 != nil:
		return "RFC 8693 " + p.RFC8693.TokenEndpointURL
	}

	return "unknown"
}

// strippedHeaders are request headers the token replaces, beyond the one the
// binding sets: the key an SDK sends when it has no other choice.
func strippedHeaders(b bindings.Binding) []string {
	if b.Provider.Anthropic != nil {
		return []string{"X-Api-Key"}
	}

	return nil
}

// Exchange starts tokenex's exchange of the subject token for the binding's
// provider. The channel delivers each new access token, and closes after an error.
func Exchange(ctx context.Context, log logr.Logger, b bindings.Binding, subject token.IdentityTokenProvider) (<-chan credential.Result, error) {
	p := b.Provider

	switch {
	case p.Anthropic != nil:
		a := p.Anthropic

		return rfc7523.GetAnthropicCredentials(ctx, log, subject, rfc7523.AnthropicWIFConfig{
			FederationRuleID: a.FederationRuleID,
			OrganizationID:   a.OrganizationID,
			ServiceAccountID: a.ServiceAccountID,
			WorkspaceID:      a.WorkspaceID,
		})
	case p.OpenAI != nil:
		o := p.OpenAI

		return rfc8693.GetOpenAICredentials(ctx, log, subject, rfc8693.OpenAIWIFConfig{
			ClientID:           o.ClientID,
			IdentityProviderID: o.IdentityProviderID,
			ServiceAccountID:   o.ServiceAccountID,
		})
	case p.RFC7523 != nil:
		return exchangeRFC7523(ctx, log, b, subject)
	case p.RFC8693 != nil:
		return exchangeRFC8693(ctx, log, b, subject)
	case p.GCP != nil:
		return exchangeGCP(ctx, log, b, subject)
	case p.Azure != nil:
		az := p.Azure

		cp, err := azure.NewCredentialsProvider(ctx, log)
		if err != nil {
			return nil, err
		}

		return cp.GetCredentials(ctx, subject,
			azure.WithTenantID(az.TenantID), azure.WithClientID(az.ClientID), azure.WithScope(az.Scope))
	}

	return nil, errNoProvider
}

func exchangeRFC7523(ctx context.Context, log logr.Logger, b bindings.Binding, subject token.IdentityTokenProvider) (<-chan credential.Result, error) {
	r := b.Provider.RFC7523

	cp, err := rfc7523.NewCredentialsProvider(ctx, log)
	if err != nil {
		return nil, err
	}

	opts := []option.Option{rfc7523.WithScopes(r.Scopes)}

	if len(r.AdditionalParams) > 0 {
		params := map[string][]string{}
		for _, p := range r.AdditionalParams {
			params[p.Key] = p.Values
		}

		opts = append(opts, rfc7523.WithAdditionalParams(params))
	}

	if r.BodyFormat == "BODY_FORMAT_JSON" {
		opts = append(opts, rfc7523.WithBodyFormat(rfc7523.BodyFormatJSON))
	}

	return cp.GetCredentials(ctx, r.TokenEndpointURL, subject, opts...)
}

func exchangeRFC8693(ctx context.Context, log logr.Logger, b bindings.Binding, subject token.IdentityTokenProvider) (<-chan credential.Result, error) {
	r := b.Provider.RFC8693

	cp, err := rfc8693.NewCredentialsProvider(ctx, log)
	if err != nil {
		return nil, err
	}

	// A service account token is a JWT; ID_TOKEN is for providers that insist.
	tokenType := rfc8693.TokenTypeJWT
	if r.SubjectTokenType == "SUBJECT_TOKEN_TYPE_ID_TOKEN" {
		tokenType = rfc8693.TokenTypeIDToken
	}

	opts := []option.Option{rfc8693.WithSubjectTokenType(tokenType), rfc8693.WithScopes(r.Scopes)}

	if r.RequestedTokenType != "" {
		opts = append(opts, rfc8693.WithRequestedTokenType(r.RequestedTokenType))
	}

	if r.Audience != "" {
		opts = append(opts, rfc8693.WithAudience(r.Audience))
	}

	if len(r.AdditionalFields) > 0 {
		opts = append(opts, rfc8693.WithAdditionalFields(r.AdditionalFields))
	}

	return cp.GetCredentials(ctx, r.TokenEndpointURL, subject, opts...)
}

func exchangeGCP(ctx context.Context, log logr.Logger, b bindings.Binding, subject token.IdentityTokenProvider) (<-chan credential.Result, error) {
	g := b.Provider.GCP

	cp, err := gcp.NewCredentialsProvider(ctx, log)
	if err != nil {
		return nil, err
	}

	// The STS audience is the provider's resource name; the subject token's
	// audience is the binding's.
	opts := []option.Option{gcp.WithAudience(g.OIDCProviderID)}

	if len(g.Scopes) > 0 {
		opts = append(opts, gcp.WithScopes(g.Scopes...))
	}

	if g.ServiceAccount != "" {
		opts = append(opts, gcp.WithServiceAccountEmail(g.ServiceAccount))
	}

	if g.Lifetime != nil {
		opts = append(opts, gcp.WithTokenLifetime(int64(g.Lifetime.Seconds())))
	}

	return cp.GetCredentials(ctx, subject, opts...)
}
