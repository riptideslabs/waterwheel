// SPDX-License-Identifier: Apache-2.0

package proxy_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.riptides.io/waterwheel/internal/bindings"
	"go.riptides.io/waterwheel/internal/proxy"
)

func TestBindingsClient(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer the-token" || r.URL.Query().Get("watch") != "true" {
			http.Error(w, "forbidden", http.StatusForbidden)

			return
		}

		enc := json.NewEncoder(w)
		for _, s := range []bindings.Snapshot{
			{Identity: testIdentity},
			{Identity: testIdentity, Bindings: []bindings.Binding{{Name: "a"}}},
		} {
			if err := enc.Encode(s); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("the-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})

	c, err := proxy.NewBindingsClient(strings.TrimPrefix(srv.URL, "https://"), tokenPath, caPEM)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	got := make(chan bindings.Snapshot, 10)
	go c.Watch(ctx, func(s bindings.Snapshot) { got <- s })

	for want := range 2 {
		select {
		case s := <-got:
			if len(s.Bindings) != want {
				t.Fatalf("snapshot %d has %d bindings", want, len(s.Bindings))
			}
		case <-ctx.Done():
			t.Fatal("no snapshot")
		}
	}
}

func TestTokenFile(t *testing.T) {
	t.Parallel()

	exp := time.Now().Add(time.Hour).Unix()
	claims, err := json.Marshal(map[string]any{"sub": "system:serviceaccount:support:bot", "exp": exp})
	if err != nil {
		t.Fatal(err)
	}

	jwt := "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, bindings.TokenFile("aud")), []byte(jwt+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tok, err := proxy.TokenFile(dir, "aud").GetToken(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if tok.Token != jwt || tok.ExpiresAt.Unix() != exp {
		t.Errorf("GetToken() = %q, %v", tok.Token, tok.ExpiresAt)
	}

	if _, err := proxy.TokenFile(dir, "other").GetToken(t.Context()); err == nil {
		t.Error("a missing token file should be an error")
	}
}
