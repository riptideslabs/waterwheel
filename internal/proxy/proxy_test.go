// SPDX-License-Identifier: Apache-2.0

package proxy_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"go.riptides.io/waterwheel/api/v1alpha1"
	"go.riptides.io/waterwheel/internal/bindings"
	"go.riptides.io/waterwheel/internal/proxy"
)

const (
	testIdentity = "ns/support/sa/bot"
	testToken    = "tok-1"
)

var errDenied = errors.New("status 401: denied")

type staticTokens struct {
	tok string
	err error
}

func (s staticTokens) Get(context.Context) (string, error) { return s.tok, s.err }

// echo answers with the request's credential headers. A WebSocket upgrade
// gets its Authorization header as the first line, then its own bytes back.
func echo(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Upgrade") == "websocket" {
		upgrade(w, r)

		return
	}

	if r.URL.Path == "/v1/duplex" {
		duplex(w, r)

		return
	}

	err := json.NewEncoder(w).Encode(map[string]string{
		"authorization": r.Header.Get("Authorization"),
		"x-api-key":     r.Header.Get("X-Api-Key"),
		"path":          r.URL.Path,
		"proto":         r.Proto,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func upgrade(w http.ResponseWriter, r *http.Request) {
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}
	defer conn.Close()

	_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n" +
		r.Header.Get("Authorization") + "\n")
	_ = brw.Flush()
	_, _ = io.Copy(conn, brw.Reader)
}

// duplex sends its headers before it reads the request, then the request's
// body back.
func duplex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Length", strconv.FormatInt(r.ContentLength, 10))
	w.WriteHeader(http.StatusOK)
	_ = http.NewResponseController(w).Flush()
	_, _ = io.Copy(w, r.Body)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// gatedBody blocks reads until open is closed.
type gatedBody struct {
	io.ReadCloser

	open <-chan struct{}
}

func (b gatedBody) Read(p []byte) (int, error) {
	<-b.open

	return b.ReadCloser.Read(p)
}

type setup struct {
	proxy  *proxy.Proxy
	client *http.Client
	bound  *httptest.Server
	other  *httptest.Server
	starts atomic.Int32
}

// newSetup runs a proxy with two upstream servers: bound gets the binding's
// token, other is passed through. Like the providers' APIs, bound offers
// HTTP/2, which the proxy prefers.
func newSetup(t *testing.T, tokens proxy.Tokens) *setup {
	t.Helper()

	s := &setup{}
	s.bound = httptest.NewUnstartedServer(http.HandlerFunc(echo))
	s.bound.EnableHTTP2 = true
	s.bound.StartTLS()
	s.other = httptest.NewTLSServer(http.HandlerFunc(echo))
	t.Cleanup(s.bound.Close)
	t.Cleanup(s.other.Close)

	ca, err := proxy.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	s.proxy = proxy.New(ca, proxy.TokenFiles(t.TempDir()))
	// The proxy trusts the bound test server's certificate upstream.
	s.proxy.Upstream = s.bound.Client().Transport
	s.proxy.NewTokens = func(context.Context, string, bindings.Binding) proxy.Tokens {
		s.starts.Add(1)

		return tokens
	}

	p := httptest.NewServer(s.proxy)
	t.Cleanup(p.Close)

	// The app trusts the pod CA (for intercepted hosts) and the passed-through
	// server's own certificate.
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	roots.AddCert(s.other.Certificate())

	proxyURL, _ := url.Parse(p.URL)
	s.client = &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
	}}

	s.proxy.Update(t.Context(), snapshot(t, s.bound.URL))

	return s
}

func snapshot(t *testing.T, boundURL string) bindings.Snapshot {
	t.Helper()

	u, _ := url.Parse(boundURL)
	host, port, _ := net.SplitHostPort(u.Host)
	p, _ := strconv.Atoi(port)

	return bindings.Snapshot{Identity: testIdentity, Bindings: []bindings.Binding{{
		Namespace:    "support",
		Name:         "bot-anthropic",
		Provider:     v1alpha1.CredentialSourceSpec{Anthropic: &v1alpha1.AnthropicWIFCredentialProvider{}},
		Audience:     "https://api.anthropic.com",
		Destinations: []v1alpha1.Destination{{Host: host, Port: int32(p)}},
		Header:       "Authorization",
		Scheme:       "Bearer",
	}}}
}

func call(t *testing.T, c *http.Client, target string) (int, map[string]string) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, target+"/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("X-Api-Key", "sk-placeholder")
	req.Header.Set("Authorization", "Bearer unused")

	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	got := map[string]string{}
	_ = json.Unmarshal(body, &got)

	return resp.StatusCode, got
}

func TestInterceptAddsToken(t *testing.T) {
	t.Parallel()

	s := newSetup(t, staticTokens{tok: testToken})

	status, got := call(t, s.client, s.bound.URL)
	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}

	if got["authorization"] != "Bearer "+testToken || got["x-api-key"] != "" || got["path"] != "/v1/messages" {
		t.Errorf("upstream saw %v; want the binding's token, no key", got)
	}

	if got["proto"] != "HTTP/2.0" {
		t.Errorf("upstream request over %s; want HTTP/2", got["proto"])
	}

	// An unchanged snapshot keeps the binding's credentials running.
	s.proxy.Update(t.Context(), snapshot(t, s.bound.URL))

	if n := s.starts.Load(); n != 1 {
		t.Errorf("credentials started %d times; want 1", n)
	}
}

// TestWebSocket upgrades over HTTP/1.1 even though bound offers HTTP/2: Go's
// transport doesn't offer h2 for a WebSocket upgrade.
func TestWebSocket(t *testing.T) {
	t.Parallel()

	s := newSetup(t, staticTokens{tok: testToken})

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, s.bound.URL+"/v1/realtime", nil)
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Authorization", "Bearer unused")

	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	conn, ok := resp.Body.(io.ReadWriter)
	if resp.StatusCode != http.StatusSwitchingProtocols || !ok {
		t.Fatalf("status %d, body %T; want 101 and a connection", resp.StatusCode, resp.Body)
	}

	r := bufio.NewReader(conn)
	if line, _ := r.ReadString('\n'); line != "Bearer "+testToken+"\n" {
		t.Errorf("upstream saw %q; want the binding's token", line)
	}

	if _, err := io.WriteString(conn, "ping\n"); err != nil {
		t.Fatal(err)
	}

	if line, _ := r.ReadString('\n'); line != "ping\n" {
		t.Errorf("read %q back; want ping", line)
	}
}

// TestBodyAfterResponseHeaders: an upstream can answer with headers before the
// proxy has sent it the request body. The body must still reach it, and its
// answer the app.
func TestBodyAfterResponseHeaders(t *testing.T) {
	t.Parallel()

	s := newSetup(t, staticTokens{tok: testToken})

	// Hold the request body back until the app has the response headers.
	open := make(chan struct{})
	upstream := s.proxy.Upstream
	s.proxy.Upstream = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.Body = gatedBody{ReadCloser: r.Body, open: open}

		return upstream.RoundTrip(r)
	})

	const body = `{"messages": [{"role": "user", "content": "hi"}]}`

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.bound.URL+"/v1/duplex", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	close(open)

	got, err := io.ReadAll(resp.Body)
	if err != nil || string(got) != body {
		t.Errorf("read %q, %v; want the request body back", got, err)
	}
}

func TestPassThrough(t *testing.T) {
	t.Parallel()

	s := newSetup(t, staticTokens{tok: testToken})

	status, got := call(t, s.client, s.other.URL)
	if status != http.StatusOK || got["authorization"] != "Bearer unused" || got["x-api-key"] != "sk-placeholder" {
		t.Errorf("status %d, upstream saw %v; want the app's own headers, untouched", status, got)
	}
}

func TestNoToken(t *testing.T) {
	t.Parallel()

	s := newSetup(t, staticTokens{err: errDenied})

	status, got := call(t, s.client, s.bound.URL)
	if status != http.StatusForbidden || got["authorization"] != "" {
		t.Errorf("status %d, body %v; want 403 without reaching upstream", status, got)
	}
}

func TestNewSPIFFEIDRestartsCredentials(t *testing.T) {
	t.Parallel()

	s := newSetup(t, staticTokens{})

	for _, id := range []string{"spiffe://waterwheel.test/a", "spiffe://waterwheel.test/a", "spiffe://waterwheel.test/b"} {
		snap := snapshot(t, s.bound.URL)
		snap.SPIFFEID = id
		s.proxy.Update(t.Context(), snap)
	}

	// The setup's snapshot, then one for each new SPIFFE ID.
	if n := s.starts.Load(); n != 3 {
		t.Errorf("credentials started %d times; want 3", n)
	}

	if got := s.proxy.SPIFFEID(); got != "spiffe://waterwheel.test/b" {
		t.Errorf("SPIFFEID() = %q", got)
	}
}

// TestSVIDParams: the pod's SPIFFE ID and the first binding's audience, which
// is empty without one.
func TestSVIDParams(t *testing.T) {
	t.Parallel()

	const spiffeID = "spiffe://waterwheel.test/svid"

	s := newSetup(t, staticTokens{})

	snap := snapshot(t, s.bound.URL)
	snap.SPIFFEID = spiffeID
	s.proxy.Update(t.Context(), snap)

	if id, aud := s.proxy.SVIDParams(); id != spiffeID || aud != "https://api.anthropic.com" {
		t.Errorf("SVIDParams() = %q, %q", id, aud)
	}

	snap.Bindings = nil
	s.proxy.Update(t.Context(), snap)

	if id, aud := s.proxy.SVIDParams(); id != spiffeID || aud != "" {
		t.Errorf("SVIDParams() without bindings = %q, %q", id, aud)
	}
}

// TestReports: the proxy reports a missing subject token once, and each new
// reason the pod has no identity.
func TestReports(t *testing.T) {
	t.Parallel()

	ca, err := proxy.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// No projected tokens at all.
	p := proxy.New(ca, proxy.TokenFiles(t.TempDir()))

	var got []string

	p.Report = func(r bindings.Report) { got = append(got, r.Reason) }

	snap := snapshot(t, "https://api.anthropic.com:443")
	ambiguous := bindings.Snapshot{Reason: "two WorkloadIdentities match"}
	for _, s := range []bindings.Snapshot{snap, snap, ambiguous, ambiguous, {Reason: "none matches"}} {
		p.Update(t.Context(), s)
	}

	want := []string{bindings.ReasonNoSubjectToken, bindings.ReasonNoWorkloadIdentity, bindings.ReasonNoWorkloadIdentity}
	if !slices.Equal(got, want) {
		t.Errorf("reports %v; want %v", got, want)
	}
}
