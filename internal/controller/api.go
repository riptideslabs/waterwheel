// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"go.riptides.io/waterwheel/internal/bindings"
)

// MaxWatch bounds a watch stream; the proxy then reconnects with a fresh
// token, which re-checks that its pod still exists.
const MaxWatch = 10 * time.Minute

// API serves each proxy its bindings:
//
//	GET /v1/bindings             one snapshot
//	GET /v1/bindings?watch=true  a snapshot per line, again on every change
//
// Requests carry the pod's projected token as a bearer token.
type API struct {
	Auth            Authenticator
	Reader          client.Reader
	SystemNamespace string
	Changes         *Broadcaster
}

func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/bindings" {
		http.NotFound(w, r)

		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

		return
	}

	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		http.Error(w, "missing bearer token", http.StatusUnauthorized)

		return
	}

	caller, err := a.Auth.Authenticate(r.Context(), token)
	if err != nil {
		slog.Info("bindings request refused", "remote", r.RemoteAddr, "reason", err)
		http.Error(w, "forbidden", http.StatusForbidden)

		return
	}

	if r.URL.Query().Get("watch") == strTrue {
		a.watch(w, r, caller)

		return
	}

	snap, err := a.snapshot(r.Context(), caller)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(snap)
}

func (a *API) snapshot(ctx context.Context, c Caller) ([]byte, error) {
	snap, err := bindings.For(ctx, a.Reader, a.SystemNamespace, c.Namespace, c.Identity())
	if err != nil {
		return nil, err
	}

	b, err := json.Marshal(snap)
	if err != nil {
		return nil, err
	}

	return append(b, '\n'), nil
}

func (a *API) watch(w http.ResponseWriter, r *http.Request, c Caller) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)

		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), MaxWatch)
	defer cancel()

	w.Header().Set("Content-Type", "application/x-ndjson")

	var last []byte

	for {
		// Take the channel before reading, so a change during the read isn't missed.
		changed := a.Changes.Changed()

		snap, err := a.snapshot(ctx, c)
		if err != nil {
			slog.Error("bindings snapshot", "pod", c.Namespace+"/"+c.Pod, "err", err)

			return
		}

		if !bytes.Equal(snap, last) {
			if _, err := w.Write(snap); err != nil {
				return
			}

			flusher.Flush()

			last = snap
		}

		select {
		case <-ctx.Done():
			return
		case <-changed:
		}
	}
}

// Broadcaster tells watchers that bindings or sources changed.
type Broadcaster struct {
	mu sync.Mutex
	ch chan struct{}
}

func NewBroadcaster() *Broadcaster {
	return &Broadcaster{ch: make(chan struct{})}
}

// Changed returns a channel that is closed on the next change.
func (b *Broadcaster) Changed() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.ch
}

// Notify wakes every watcher.
func (b *Broadcaster) Notify() {
	b.mu.Lock()
	defer b.mu.Unlock()

	close(b.ch)
	b.ch = make(chan struct{})
}
