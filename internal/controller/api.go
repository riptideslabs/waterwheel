// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"go.riptides.io/waterwheel/api/v1alpha1"
	"go.riptides.io/waterwheel/internal/bindings"
	"go.riptides.io/waterwheel/internal/identity"
)

// MaxWatch bounds a watch stream; the proxy then reconnects with a fresh
// token, which re-checks that its pod still exists.
const MaxWatch = 10 * time.Minute

// API serves each proxy its bindings, and records what it reports as events
// on its pod:
//
//	GET  /v1/bindings             one snapshot
//	GET  /v1/bindings?watch=true  a snapshot per line, again on every change
//	POST /v1/reports              a bindings.Report
//
// Requests carry the pod's projected token as a bearer token.
type API struct {
	Auth            Authenticator
	Reader          client.Reader
	SystemNamespace string
	Changes         *Broadcaster
	// TrustDomain is set with SPIFFE sources: the pod's workload ID comes
	// from the WorkloadIdentities, rather than its service account.
	TrustDomain string
	// Recorder gets the events of reports.
	Recorder events.EventRecorder
	// ReportTimeout bounds how long a report may take to arrive; 10 seconds if zero.
	ReportTimeout time.Duration

	mu      sync.Mutex
	watches map[[sha256.Size]byte]*openWatch // by token
}

// openWatch is a watch's caller, and the snapshot it sent last.
type openWatch struct {
	caller Caller
	snap   bindings.Snapshot
}

// reportEvents are the reasons a proxy may report, with their event's type and action.
var reportEvents = map[string]struct{ typ, action string }{
	bindings.ReasonCredentialIssued:    {corev1.EventTypeNormal, actionExchangeToken},
	bindings.ReasonTokenExchangeFailed: {corev1.EventTypeWarning, actionExchangeToken},
	bindings.ReasonNoSubjectToken:      {corev1.EventTypeWarning, actionExchangeToken},
	bindings.ReasonNoWorkloadIdentity:  {corev1.EventTypeWarning, actionResolveIdentity},
}

func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var method string

	switch r.URL.Path {
	case "/v1/bindings":
		method = http.MethodGet
	case "/v1/reports":
		method = http.MethodPost
	default:
		http.NotFound(w, r)

		return
	}

	if r.Method != method {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

		return
	}

	// A report is small, so bound how long its body may take. Watches are
	// GETs without a body, and keep no deadline.
	if method == http.MethodPost {
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(cmp.Or(a.ReportTimeout, 10*time.Second)))
	}

	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		http.Error(w, "missing bearer token", http.StatusUnauthorized)

		return
	}

	if method == http.MethodPost {
		a.report(w, r, token)

		return
	}

	caller, ok := a.authenticate(w, r, token)
	if !ok {
		return
	}

	if r.URL.Query().Get("watch") == strTrue {
		a.watch(w, r, token, caller)

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

func (a *API) authenticate(w http.ResponseWriter, r *http.Request, token string) (Caller, bool) {
	caller, err := a.Auth.Authenticate(r.Context(), token)
	if err != nil {
		slog.Info("bindings request refused", "remote", r.RemoteAddr, "reason", err)
		http.Error(w, "forbidden", http.StatusForbidden)

		return Caller{}, false
	}

	return caller, true
}

func (a *API) snapshot(ctx context.Context, c Caller) ([]byte, error) {
	snap, err := a.bindingsOf(ctx, c)
	if err != nil {
		return nil, err
	}

	return encode(snap)
}

func encode(snap bindings.Snapshot) ([]byte, error) {
	b, err := json.Marshal(snap)
	if err != nil {
		return nil, err
	}

	return append(b, '\n'), nil
}

func (a *API) bindingsOf(ctx context.Context, c Caller) (bindings.Snapshot, error) {
	if a.TrustDomain == "" {
		return bindings.For(ctx, a.Reader, a.SystemNamespace, c.Namespace, c.Identity())
	}

	var wids v1alpha1.WorkloadIdentityList
	if err := a.Reader.List(ctx, &wids); err != nil {
		return bindings.Snapshot{}, err
	}

	id, reason := resolve(identity.Build(wids.Items, a.SystemNamespace), c)
	if reason != "" {
		// No identity, so nothing to exchange; the proxy logs why.
		return bindings.Snapshot{Bindings: []bindings.Binding{}, Reason: reason}, nil
	}

	snap, err := bindings.For(ctx, a.Reader, a.SystemNamespace, c.Namespace, id)
	snap.SPIFFEID = identity.SPIFFEID(a.TrustDomain, id)

	return snap, err
}

// report records an event on the caller's pod, about one of its bindings or
// about its identity. Only the controller decides the event's type, and why a
// pod has no identity.
//
// A proxy reports about what its watch sent it. A report with the token of an
// open watch takes that watch's caller and snapshot, rather than reviewing the
// token and resolving the pod's bindings again.
func (a *API) report(w http.ResponseWriter, r *http.Request, token string) {
	c, snap, watched := a.watched(token)
	if !watched {
		var ok bool
		if c, ok = a.authenticate(w, r, token); !ok {
			return
		}
	}

	var rep bindings.Report
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&rep); err != nil {
		http.Error(w, "bad report", http.StatusBadRequest)

		return
	}

	ev, ok := reportEvents[rep.Reason]
	if !ok {
		http.Error(w, "unknown reason", http.StatusBadRequest)

		return
	}

	if !watched {
		var err error
		if snap, err = a.bindingsOf(r.Context(), c); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)

			return
		}
	}

	note := rep.Message

	var related runtime.Object

	if rep.Reason == bindings.ReasonNoWorkloadIdentity {
		if snap.Reason == "" {
			http.Error(w, "the pod has a workload identity", http.StatusConflict)

			return
		}

		note = snap.Reason
	} else {
		ns, name, _ := strings.Cut(rep.Binding, "/")
		if !slices.ContainsFunc(snap.Bindings, func(b bindings.Binding) bool { return b.Namespace == ns && b.Name == name }) {
			http.Error(w, "not one of the pod's bindings", http.StatusForbidden)

			return
		}

		var cb v1alpha1.CredentialBinding
		if err := a.Reader.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &cb); err == nil {
			related = &cb
		}
	}

	if a.Recorder != nil {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: c.Namespace, Name: c.Pod, UID: c.PodUID}}
		eventf(a.Recorder, pod, related, ev.typ, rep.Reason, ev.action, note)
	}

	w.WriteHeader(http.StatusNoContent)
}

// resolve returns the caller's workload ID, or why it has none.
func resolve(set *identity.Set, c Caller) (string, string) {
	id, err := set.Resolve(c.Selectable())
	if err != nil {
		return "", err.Error()
	}

	return id, ""
}

// watched returns the caller and latest snapshot of the open watch with token.
func (a *API) watched(token string) (Caller, bindings.Snapshot, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if o, ok := a.watches[sha256.Sum256([]byte(token))]; ok {
		return o.caller, o.snap, true
	}

	return Caller{}, bindings.Snapshot{}, false
}

func (a *API) watch(w http.ResponseWriter, r *http.Request, token string, c Caller) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)

		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), MaxWatch)
	defer cancel()

	w.Header().Set("Content-Type", "application/x-ndjson")

	key := sha256.Sum256([]byte(token))
	open := &openWatch{caller: c}

	defer func() {
		a.mu.Lock()
		defer a.mu.Unlock()

		if a.watches[key] == open {
			delete(a.watches, key)
		}
	}()

	var last []byte

	for {
		// Take the channel before reading, so a change during the read isn't missed.
		changed := a.Changes.Changed()

		snap, err := a.bindingsOf(ctx, c)

		var line []byte
		if err == nil {
			line, err = encode(snap)
		}

		if err != nil {
			slog.Error("bindings snapshot", "pod", c.Namespace+"/"+c.Pod, "err", err)

			return
		}

		// Before it's sent, so that reports about it find it.
		a.mu.Lock()
		open.snap = snap
		if a.watches == nil {
			a.watches = map[[sha256.Size]byte]*openWatch{}
		}

		a.watches[key] = open
		a.mu.Unlock()

		if !bytes.Equal(line, last) {
			if _, err := w.Write(line); err != nil {
				return
			}

			flusher.Flush()

			last = line
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
