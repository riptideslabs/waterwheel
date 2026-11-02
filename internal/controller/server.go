// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

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
