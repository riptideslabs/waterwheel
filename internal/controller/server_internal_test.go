// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// bodyReader reads request bodies whole, as the admission handler does, and
// keeps the size of the largest.
type bodyReader struct{ largest atomic.Int64 }

func (b *bodyReader) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if int64(len(body)) > b.largest.Load() {
		b.largest.Store(int64(len(body)))
	}

	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}

func startWebhookServer(t *testing.T, handler http.Handler, readTimeout time.Duration) *httptest.Server {
	t.Helper()

	srv := NewWebhookServer("", handler, nil)
	if srv.ReadTimeout <= 0 || srv.WriteTimeout <= 0 || srv.IdleTimeout <= 0 {
		t.Fatalf("no timeout: read %s, write %s, idle %s", srv.ReadTimeout, srv.WriteTimeout, srv.IdleTimeout)
	}

	// The real timeout is too long for a test.
	srv.ReadTimeout = readTimeout

	ts := httptest.NewUnstartedServer(nil)
	ts.Config = srv
	ts.StartTLS()
	t.Cleanup(ts.Close)

	return ts
}

// A client that sends part of a body and stops must not keep it in the
// controller's memory for as long as it likes.
func TestWebhookServerDropsStalledBody(t *testing.T) {
	t.Parallel()

	ts := startWebhookServer(t, &bodyReader{}, 300*time.Millisecond)

	dialer := &tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}}}

	conn, err := dialer.DialContext(t.Context(), "tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	const size = 1 << 20

	fmt.Fprintf(conn, "POST /inject HTTP/1.1\r\nHost: x\r\nContent-Length: %d\r\n\r\n", size)

	if _, err := conn.Write(make([]byte, size-1)); err != nil {
		t.Fatal(err)
	}

	// The server answers or closes once the read times out.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	if _, err := io.ReadAll(conn); err != nil {
		t.Fatalf("the server kept waiting for the body: %v", err)
	}
}

func TestWebhookServerLimitsBody(t *testing.T) {
	t.Parallel()

	reader := &bodyReader{}
	ts := startWebhookServer(t, reader, 10*time.Second)

	body := bytes.NewReader(make([]byte, webhookMaxBody+1<<20))

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/inject", body)
	if err != nil {
		t.Fatal(err)
	}

	// The server may close the connection before the client has sent it all.
	if resp, err := ts.Client().Do(req); err == nil {
		_ = resp.Body.Close()
	}

	if got := reader.largest.Load(); got > webhookMaxBody {
		t.Errorf("read %d bytes of a body, over the limit of %d", got, webhookMaxBody)
	}
}

func TestLimitInFlight(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	started := make(chan struct{}, 2)
	h := limitInFlight(2, 100*time.Millisecond, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		started <- struct{}{}

		<-release
	}))

	serve := func() int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/inject", http.NoBody))

		return rec.Code
	}

	codes := make(chan int, 2)

	for range 2 {
		go func() { codes <- serve() }()
	}

	<-started
	<-started

	if code := serve(); code != http.StatusServiceUnavailable {
		t.Errorf("a third request got %d while two were in flight", code)
	}

	close(release)

	for range 2 {
		if code := <-codes; code != http.StatusOK {
			t.Errorf("a request in flight got %d", code)
		}
	}

	if code := serve(); code != http.StatusOK {
		t.Errorf("a request got %d after the others finished", code)
	}
}
