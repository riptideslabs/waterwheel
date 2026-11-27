// SPDX-License-Identifier: Apache-2.0

package proxy_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"go.riptides.io/tokenex/pkg/credential"
	"go.riptides.io/tokenex/pkg/option"
	"go.riptides.io/tokenex/pkg/token"
	"go.riptides.io/waterwheel/api/v1alpha1"
	"go.riptides.io/waterwheel/internal/bindings"
	"go.riptides.io/waterwheel/internal/proxy"
)

// An rfc8693 source's request is JSON, as the docs say; tokenex's own default
// is form encoding.
func TestRFC8693ExchangeSendsJSON(t *testing.T) {
	t.Parallel()

	const subjectToken, accessToken = "subject-token", "access-token"

	requests := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["subject_token"] != subjectToken {
			http.Error(w, "not a JSON token exchange request", http.StatusBadRequest)
		} else {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":      accessToken,
				"issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
				"token_type":        "Bearer",
				"expires_in":        3600,
			})
		}

		select {
		case requests <- r.Header.Get("Content-Type"):
		default:
		}
	}))
	t.Cleanup(srv.Close)

	subject := token.IdentityTokenProviderFunc(func(context.Context, ...option.Option) (credential.Token, error) {
		return credential.Token{Token: subjectToken, ExpiresAt: time.Now().Add(time.Hour)}, nil
	})

	ch, err := proxy.Exchange(t.Context(), logr.Discard(), bindings.Binding{
		Provider: v1alpha1.CredentialSourceSpec{RFC8693: &v1alpha1.RFC8693CredentialProvider{TokenEndpointURL: srv.URL}},
	}, subject)
	if err != nil {
		t.Fatal(err)
	}

	res := <-ch
	if contentType := <-requests; contentType != "application/json" {
		t.Errorf("Content-Type = %q", contentType)
	}

	if res.Err != nil {
		t.Fatal(res.Err)
	}

	if tok, ok := res.Credential.(*credential.Oauth2Creds); !ok || tok.AccessToken != accessToken {
		t.Errorf("credential = %#v", res.Credential)
	}
}
