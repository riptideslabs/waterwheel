// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.riptides.io/waterwheel/api/v1alpha1"
	"go.riptides.io/waterwheel/internal/bindings"
)

// A connection intercepted while a binding applied must not keep getting the
// binding's token after it is removed.
func TestRemovedBindingStopsServingItsToken(t *testing.T) {
	t.Parallel()

	const token = "provider-token"

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Header.Get("Authorization"))
	}))
	t.Cleanup(upstream.Close)

	ca, err := LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	p := New(ca, TokenFiles(t.TempDir()))
	p.Upstream = upstream.Client().Transport
	// Real credentials, with a token as if an exchange had delivered it. The
	// binding names no provider, so run only waits for its route to end.
	p.NewTokens = func(ctx context.Context, _ string, b bindings.Binding) Tokens {
		c := newCredentials(ctx.Done(), b, nil, slog.New(slog.DiscardHandler), nil)
		c.set(token, time.Now().Add(time.Hour))

		go c.run(ctx)

		return c
	}

	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	roots.AddCert(upstream.Certificate())

	proxyURL, _ := url.Parse(front.URL)
	client := &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
	}}

	get := func() (int, string, bool) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, upstream.URL, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}

		req.Header.Set("Authorization", "Bearer the-apps-own")

		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)

		return resp.StatusCode, string(body), resp.Close
	}

	u, _ := url.Parse(upstream.URL)
	host, portStr, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(portStr)

	p.Update(t.Context(), bindings.Snapshot{Identity: "ns/support/sa/bot", Bindings: []bindings.Binding{{
		Namespace: "support", Name: "bot", Header: "Authorization",
		Destinations: []v1alpha1.Destination{{Host: host, Port: int32(port)}},
	}}})

	if status, body, _ := get(); status != http.StatusOK || body != token {
		t.Fatalf("with the binding: status %d, Authorization %q", status, body)
	}

	p.Update(t.Context(), bindings.Snapshot{Identity: "ns/support/sa/bot"})

	// The connection is still the intercepted one, and the route ended with
	// the update: not one more request gets the token.
	status, body, closing := get()
	if status != http.StatusForbidden {
		t.Fatalf("after the binding was removed: status %d, Authorization %q", status, body)
	}

	if !closing {
		t.Error("the proxy kept the connection of a route that is gone")
	}

	// A new connection is passed through, with what the app sent.
	if status, body, _ := get(); status != http.StatusOK || strings.Contains(body, token) {
		t.Errorf("on a new connection: status %d, Authorization %q", status, body)
	}
}

// The route ends when its context does, not a moment later: a request that
// asks right after gets no token.
func TestCredentialsEndWithTheirRoute(t *testing.T) {
	t.Parallel()

	for range 1000 {
		ctx, cancel := context.WithCancel(t.Context())

		c := newCredentials(ctx.Done(), bindings.Binding{}, nil, slog.New(slog.DiscardHandler), nil)
		c.set("provider-token", time.Now().Add(time.Hour))

		if tok, err := c.Get(t.Context()); err != nil || tok == "" {
			t.Fatalf("before the route ended: %q, %v", tok, err)
		}

		cancel()

		if tok, err := c.Get(t.Context()); !gone(err) {
			t.Fatalf("after the route ended: %q, %v", tok, err)
		}
	}
}
