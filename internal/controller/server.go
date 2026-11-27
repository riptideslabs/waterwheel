// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"time"
)

// Limits of the servers. Anyone who can reach their ports can open
// connections, so what a request may hold in memory, and for how long, is
// bounded before it is authenticated.
const (
	headerTimeout = 10 * time.Second
	idleTimeout   = 2 * time.Minute
	// maxHeaderBytes fits a projected token several times over.
	maxHeaderBytes = 32 << 10

	// The API server sends an AdmissionReview at once, and a pod's is far
	// smaller than this.
	webhookTimeout = 10 * time.Second
	webhookMaxBody = 4 << 20
	// A review takes milliseconds, so more than this are never served at once
	// unless their bodies are held back.
	webhookMaxInFlight = 16
	webhookMaxWait     = 2 * time.Second
)

// NewWebhookServer serves admission requests, a few at a time, each with a
// bounded body and a deadline.
func NewWebhookServer(addr string, handler http.Handler, tlsConfig *tls.Config) *http.Server {
	handler = http.MaxBytesHandler(handler, webhookMaxBody)

	return &http.Server{
		Addr:              addr,
		Handler:           limitInFlight(webhookMaxInFlight, webhookMaxWait, handler),
		ReadHeaderTimeout: headerTimeout,
		ReadTimeout:       webhookTimeout,
		WriteTimeout:      webhookTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		TLSConfig:         tlsConfig,
	}
}

// NewAPIServer serves the bindings API. Watches are long-lived, so it has no
// read or write timeout; the API bounds a report's body itself.
func NewAPIServer(addr string, handler http.Handler, tlsConfig *tls.Config) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: headerTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		TLSConfig:         tlsConfig,
	}
}

// limitInFlight serves at most n requests at once. Another waits for a free
// place, and gets a 503 when none comes up.
func limitInFlight(n int, wait time.Duration, next http.Handler) http.Handler {
	places := make(chan struct{}, n)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		timer := time.NewTimer(wait)
		defer timer.Stop()

		select {
		case places <- struct{}{}:
			defer func() { <-places }()

			next.ServeHTTP(w, r)
		case <-timer.C:
			http.Error(w, "too many requests in flight", http.StatusServiceUnavailable)
		case <-r.Context().Done():
		}
	})
}

// TLSServer runs an HTTPS server as a manager Runnable, on every replica.
type TLSServer struct {
	Server *http.Server
}

func (s *TLSServer) Start(ctx context.Context) error {
	// Requests, and so watch streams, end when the manager stops.
	s.Server.BaseContext = func(net.Listener) context.Context { return ctx }

	go func() {
		<-ctx.Done()

		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()

		_ = s.Server.Shutdown(shutdown)
	}()

	if err := s.Server.ListenAndServeTLS("", ""); !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
}

// NeedLeaderElection is false: every replica serves.
func (s *TLSServer) NeedLeaderElection() bool { return false }
