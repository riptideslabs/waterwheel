// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"cmp"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.riptides.io/waterwheel/internal/bindings"
)

// Proxy is the CONNECT proxy the app reaches through HTTPS_PROXY. For a bound
// destination it terminates TLS with a leaf from the pod CA and adds the
// binding's token to every request; everything else is tunneled untouched.
type Proxy struct {
	CA *CA
	// Upstream sends intercepted requests on.
	Upstream http.RoundTripper
	// Dialer opens tunnels.
	Dialer *net.Dialer
	// NewTokens starts the credentials of a binding for the pod's SPIFFE ID
	// (empty with service account tokens); it runs until ctx ends.
	NewTokens func(ctx context.Context, spiffeID string, b bindings.Binding) Tokens
	// Report, if set, tells the controller about changes, for events on the
	// pod. It must not block.
	Report func(bindings.Report)

	mu       sync.RWMutex
	identity string
	spiffeID string
	reason   string
	routes   []*route
	ready    chan struct{}
	once     sync.Once
}

type route struct {
	binding bindings.Binding
	tokens  Tokens
	cancel  context.CancelFunc
}

// New returns a proxy whose bindings get tokens from tokenex, exchanging the
// subject tokens from subjects.
func New(ca *CA, subjects Subjects) *Proxy {
	p := &Proxy{
		CA: ca,
		Upstream: &http.Transport{
			Proxy:               nil,
			ForceAttemptHTTP2:   true,
			TLSHandshakeTimeout: 10 * time.Second,
			IdleConnTimeout:     90 * time.Second,
			MaxIdleConns:        10,
		},
		Dialer: &net.Dialer{Timeout: 10 * time.Second},
	}

	p.NewTokens = func(ctx context.Context, spiffeID string, b bindings.Binding) Tokens {
		log := slog.With("binding", b.Namespace+"/"+b.Name, "provider", ProviderName(b))

		subject, err := subjects(spiffeID, b)
		if err != nil {
			log.Warn("no subject token", "err", err)
			p.report(bindings.Report{
				Binding: b.Namespace + "/" + b.Name, Reason: bindings.ReasonNoSubjectToken,
				Message: fmt.Sprintf("no subject token to exchange with %s: %s", ProviderName(b), describe(err)),
			})

			return failed{err}
		}

		c := newCredentials(b, subject, log, p.report)

		go c.run(ctx)

		return c
	}

	return p
}

func (p *Proxy) report(r bindings.Report) {
	if p.Report != nil {
		p.Report(r)
	}
}

// SPIFFEID is the pod's SPIFFE ID from the latest snapshot, with SPIFFE sources.
func (p *Proxy) SPIFFEID() string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return p.spiffeID
}

// SVIDParams is the pod's SPIFFE ID and its first binding's audience, if any,
// from the same snapshot.
func (p *Proxy) SVIDParams() (string, string) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if len(p.routes) == 0 {
		return p.spiffeID, ""
	}

	return p.spiffeID, p.routes[0].binding.Audience
}

// Ready is closed once the first bindings snapshot has arrived.
func (p *Proxy) Ready() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.ready == nil {
		p.ready = make(chan struct{})
	}

	return p.ready
}

// Update applies a bindings snapshot. Unchanged bindings keep their tokens;
// changed and new ones start their exchange; removed ones stop. A new SPIFFE
// ID changes every binding's subject token, so they all start over.
func (p *Proxy) Update(ctx context.Context, snap bindings.Snapshot) {
	p.mu.Lock()

	old := map[string]*route{}
	if snap.SPIFFEID == p.spiffeID {
		for _, r := range p.routes {
			old[r.binding.Namespace+"/"+r.binding.Name] = r
		}
	} else {
		for _, r := range p.routes {
			r.cancel()
		}
	}

	routes := make([]*route, 0, len(snap.Bindings))

	for _, b := range snap.Bindings {
		key := b.Namespace + "/" + b.Name
		if r, ok := old[key]; ok && reflect.DeepEqual(r.binding, b) {
			routes = append(routes, r)
			delete(old, key)

			continue
		}

		rctx, cancel := context.WithCancel(ctx)
		routes = append(routes, &route{binding: b, tokens: p.NewTokens(rctx, snap.SPIFFEID, b), cancel: cancel})
	}

	for _, r := range old {
		r.cancel()
	}

	p.identity, p.spiffeID, p.routes = cmp.Or(snap.SPIFFEID, snap.Identity), snap.SPIFFEID, routes

	newReason := snap.Reason != p.reason
	p.reason = snap.Reason

	if p.ready == nil {
		p.ready = make(chan struct{})
	}

	p.mu.Unlock()

	if newReason && snap.Reason != "" {
		p.report(bindings.Report{Reason: bindings.ReasonNoWorkloadIdentity, Message: snap.Reason})
	}

	p.once.Do(func() { close(p.ready) })

	hosts := make([]string, 0, len(routes))
	for _, r := range routes {
		for _, d := range r.binding.Destinations {
			hosts = append(hosts, d.Host+":"+strconv.Itoa(int(d.Port)))
		}
	}

	if snap.Reason != "" {
		slog.Warn("no identity, so no bindings", "reason", snap.Reason)

		return
	}

	slog.Info("bindings", "identity", cmp.Or(snap.SPIFFEID, snap.Identity), "count", len(routes),
		"intercepting", strings.Join(hosts, ", "))
}

// match returns the route for a destination, or nil to tunnel it.
func (p *Proxy) match(host string, port int) (*route, string) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	host = strings.ToLower(strings.TrimSuffix(host, "."))

	for _, r := range p.routes {
		for _, d := range r.binding.Destinations {
			if int(d.Port) == port && matchHost(d.Host, host) {
				return r, p.identity
			}
		}
	}

	return nil, p.identity
}

// matchHost: an exact name, or *.domain for any name under domain (not domain itself).
func matchHost(pattern, host string) bool {
	if suffix, ok := strings.CutPrefix(pattern, "*."); ok {
		return strings.HasSuffix(host, "."+suffix)
	}

	return host == pattern
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "waterwheel-proxy: only HTTPS (CONNECT) is supported", http.StatusMethodNotAllowed)

		return
	}

	host, portStr, err := net.SplitHostPort(r.Host)
	port, perr := strconv.Atoi(portStr)

	if err != nil || perr != nil {
		http.Error(w, "waterwheel-proxy: bad CONNECT target", http.StatusBadRequest)

		return
	}

	rt, identity := p.match(host, port)
	if rt == nil {
		p.tunnel(w, r)

		return
	}

	p.intercept(w, host, rt, identity)
}

// tunnel connects the app straight to the destination.
func (p *Proxy) tunnel(w http.ResponseWriter, r *http.Request) {
	upstream, err := p.Dialer.DialContext(r.Context(), "tcp", r.Host)
	if err != nil {
		slog.Info("CONNECT failed", "target", r.Host, "err", err)
		http.Error(w, "waterwheel-proxy: "+err.Error(), http.StatusBadGateway)

		return
	}

	client, ok := hijack(w)
	if !ok {
		_ = upstream.Close()

		return
	}

	slog.Info("CONNECT passed through", "target", r.Host)

	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)

		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}

		done <- struct{}{}
	}

	go pipe(upstream, client)
	go pipe(client, upstream)

	<-done
	<-done

	_ = client.Close()
	_ = upstream.Close()
}

// hijack takes over the client connection and confirms the CONNECT.
func hijack(w http.ResponseWriter) (net.Conn, bool) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)

		return nil, false
	}

	conn, _, err := hj.Hijack()
	if err != nil {
		return nil, false
	}

	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		_ = conn.Close()

		return nil, false
	}

	return conn, true
}

// intercept terminates TLS with a leaf from the pod CA and serves HTTP/1.1,
// WebSocket upgrades included, on the decrypted connection until the client
// closes it.
func (p *Proxy) intercept(w http.ResponseWriter, host string, rt *route, identity string) {
	cert, err := p.CA.Leaf(host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	conn, ok := hijack(w)
	if !ok {
		return
	}

	tlsConn := tls.Server(conn, &tls.Config{
		Certificates: []tls.Certificate{*cert},
		NextProtos:   []string{"http/1.1"},
		MinVersion:   tls.VersionTLS12,
	})

	ln := newConnListener(tlsConn)
	srv := &http.Server{
		Handler:           p.forwarder(rt, identity),
		ReadHeaderTimeout: 30 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
		ConnState: func(_ net.Conn, s http.ConnState) {
			if s == http.StateClosed || s == http.StateHijacked {
				_ = ln.Close()
			}
		},
	}
	_ = srv.Serve(ln)
}

type tokenKey struct{}

// forwarder sends each request on to the destination with the binding's token.
func (p *Proxy) forwarder(rt *route, identity string) http.Handler {
	b := rt.binding

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "https"
			pr.Out.URL.Host = pr.In.Host
			pr.Out.Host = pr.In.Host

			for _, h := range strippedHeaders(b) {
				pr.Out.Header.Del(h)
			}

			tok, _ := pr.In.Context().Value(tokenKey{}).(string)
			if b.Scheme != "" {
				tok = b.Scheme + " " + tok
			}

			pr.Out.Header.Set(b.Header, tok)
		},
		Transport:     p.Upstream,
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			slog.Warn("upstream error", "err", err)
			writeError(w, http.StatusBadGateway, "api_error", "waterwheel-proxy: upstream error: "+err.Error())
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		target := r.Method + " https://" + r.Host + r.URL.Path

		tok, err := rt.tokens.Get(r.Context())
		if err != nil {
			slog.Warn("refused: no token", "request", target, "identity", identity, "provider", ProviderName(b), "err", describe(err))
			writeError(w, http.StatusForbidden, "permission_error",
				fmt.Sprintf("waterwheel-proxy: %s did not issue a token to %s (%s)", ProviderName(b), identity, describe(err)))

			return
		}

		// Otherwise the server drains and closes the request body once the
		// response headers go out, failing an upstream request still sending it.
		_ = http.NewResponseController(w).EnableFullDuplex()

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		rp.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), tokenKey{}, tok)))

		slog.Info("added token", "request", target, "token", redact(tok), "identity", identity,
			"status", rec.status, "took", time.Since(start).Round(time.Millisecond))
	})
}

// writeError answers in the shape of Anthropic's API errors; OpenAI clients
// read error.message too.
func writeError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}

	body.Type, body.Error.Type, body.Error.Message = "error", typ, msg
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Debug("writing an error response", "err", err)
	}
}

type statusRecorder struct {
	http.ResponseWriter

	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack lets the reverse proxy switch protocols, for WebSockets.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	r.status = http.StatusSwitchingProtocols

	return http.NewResponseController(r.ResponseWriter).Hijack()
}

// connListener hands one connection to http.Server, then blocks until closed.
type connListener struct {
	conn net.Conn
	once sync.Once
	ch   chan net.Conn
	done chan struct{}
}

func newConnListener(c net.Conn) *connListener {
	l := &connListener{conn: c, ch: make(chan net.Conn, 1), done: make(chan struct{})}
	l.ch <- c

	return l
}

func (l *connListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *connListener) Close() error {
	l.once.Do(func() { close(l.done) })

	return nil
}

func (l *connListener) Addr() net.Addr { return l.conn.LocalAddr() }
