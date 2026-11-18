// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"crypto/tls"
	"net"
	"net/http/httptrace"
	"strconv"
	"sync"
	"time"
)

// upstreamDialer opens the proxy's connections to bound destinations, as Go's
// default transport does.
var upstreamDialer = &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}

// dialUpstream opens a connection to a bound destination, measured while it
// lasts.
func (p *Proxy) dialUpstream(ctx context.Context, network, addr string) (net.Conn, error) {
	conn, err := upstreamDialer.DialContext(ctx, network, addr)
	if err != nil || p.Metrics == nil {
		return conn, err
	}

	host, port, _ := net.SplitHostPort(addr)
	n, _ := strconv.Atoi(port)

	return &upstreamConn{Conn: conn, metrics: p.Metrics, host: host, port: n, opened: time.Now()}, nil
}

// upstreamConn is a connection to a bound destination, measured as
// OpenTelemetry's HTTP client connections are: open, and active while it
// carries a request or idle, from its first request until it closes.
type upstreamConn struct {
	net.Conn

	metrics *Metrics
	host    string
	port    int
	opened  time.Time

	mu      sync.Mutex
	version string
	active  int
	counted bool
	closed  bool
}

// begin marks the start of a request on c, whose protocol version is version.
func (c *upstreamConn) begin(ctx context.Context, version string) {
	if c == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	switch {
	case c.closed:
		return
	case !c.counted:
		c.version, c.counted = version, true
		c.metrics.connection(ctx, c, connActive, 1)
	case c.active == 0:
		c.metrics.connection(ctx, c, connIdle, -1)
		c.metrics.connection(ctx, c, connActive, 1)
	}

	c.active++
}

// end marks the end of a request begin started.
func (c *upstreamConn) end(ctx context.Context) {
	if c == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed || c.active == 0 {
		return
	}

	c.active--

	if c.active == 0 {
		c.metrics.connection(ctx, c, connActive, -1)
		c.metrics.connection(ctx, c, connIdle, 1)
	}
}

func (c *upstreamConn) Close() error {
	c.mu.Lock()

	if !c.closed {
		c.closed = true

		if c.counted {
			state := connIdle
			if c.active > 0 {
				state = connActive
			}

			c.metrics.connection(context.Background(), c, state, -1)
		}

		c.metrics.connectionClosed(context.Background(), c, time.Since(c.opened))
	}

	c.mu.Unlock()

	return c.Conn.Close()
}

// peer is the address c is connected to.
func (c *upstreamConn) peer() string {
	return peerAddress(c.RemoteAddr())
}

func peerAddress(a net.Addr) string {
	if a == nil {
		return ""
	}

	host, _, err := net.SplitHostPort(a.String())
	if err != nil {
		return ""
	}

	return host
}

// upstreamConnOf is the upstreamConn under conn, as Go's transport hands it
// over, wrapped in TLS.
func upstreamConnOf(conn net.Conn) *upstreamConn {
	for conn != nil {
		switch c := conn.(type) {
		case *upstreamConn:
			return c
		case interface{ NetConn() net.Conn }:
			conn = c.NetConn()
		default:
			return nil
		}
	}

	return nil
}

// protocolVersion is the HTTP version conn speaks: 2 if TLS negotiated it.
func protocolVersion(conn net.Conn) string {
	if tc, ok := conn.(*tls.Conn); ok && tc.ConnectionState().NegotiatedProtocol == "h2" {
		return "2"
	}

	return "1.1"
}

// upstreamTrace follows the connection a request goes out on.
type upstreamTrace struct {
	mu      sync.Mutex
	conn    *upstreamConn
	version string
}

func (t *upstreamTrace) trace(ctx context.Context) context.Context {
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			t.mu.Lock()
			defer t.mu.Unlock()

			// The transport retries on another connection when a reused one
			// turns out dead.
			t.conn.end(ctx)

			t.conn, t.version = upstreamConnOf(info.Conn), protocolVersion(info.Conn)
			t.conn.begin(ctx, t.version)
		},
	})
}

// done ends the request on its connection, and returns the protocol version
// it used, "" if it never got one.
func (t *upstreamTrace) done(ctx context.Context) string {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.conn.end(ctx)
	t.conn = nil

	return t.version
}
