// SPDX-License-Identifier: Apache-2.0

package controller_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"go.riptides.io/waterwheel/api/v1alpha1"
	"go.riptides.io/waterwheel/internal/bindings"
	"go.riptides.io/waterwheel/internal/controller"
)

const podUID = "8e9c1b54-2f7a-4d0c-9a51-3c6d2b7e4f10"

type event struct {
	regarding, related        runtime.Object
	typ, reason, action, note string
}

// recorder keeps the events it gets.
type recorder struct {
	mu     sync.Mutex
	events []event
}

func (r *recorder) Eventf(regarding, related runtime.Object, typ, reason, action, note string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.events = append(r.events, event{regarding, related, typ, reason, action, fmt.Sprintf(note, args...)})
}

// take returns the events since the last take.
func (r *recorder) take() []event {
	r.mu.Lock()
	defer r.mu.Unlock()

	ev := r.events
	r.events = nil

	return ev
}

// reasons are the events' types and reasons.
func reasons(events []event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.typ+" "+e.reason)
	}

	return out
}

func post(t *testing.T, url, token string, rep bindings.Report) int {
	t.Helper()

	body, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url+"/v1/reports", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()

	return resp.StatusCode
}

func anthropicSource() *v1alpha1.CredentialSource {
	return &v1alpha1.CredentialSource{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: sourceName},
		Spec:       v1alpha1.CredentialSourceSpec{Anthropic: &v1alpha1.AnthropicWIFCredentialProvider{}},
	}
}

func TestReports(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	srv := httptest.NewServer(&controller.API{
		Auth: fakeAuth{}, Reader: newClient(t, anthropicSource(), newBinding("first")),
		SystemNamespace: systemNamespace, Changes: controller.NewBroadcaster(), Recorder: rec,
	})
	t.Cleanup(srv.Close)

	issued := "Anthropic issued a token, valid for 1h0m0s"

	for _, tc := range []struct {
		name, token string
		rep         bindings.Report
		want        int
	}{
		{"issued", goodToken, bindings.Report{Binding: namespace + "/first", Reason: bindings.ReasonCredentialIssued, Message: issued}, http.StatusNoContent},
		{"long", goodToken, bindings.Report{
			Binding: namespace + "/first", Reason: bindings.ReasonTokenExchangeFailed, Message: strings.Repeat("é", 1000),
		}, http.StatusNoContent},
		{"bad token", "bad", bindings.Report{Binding: namespace + "/first", Reason: bindings.ReasonCredentialIssued}, http.StatusForbidden},
		{"another pod's binding", goodToken, bindings.Report{
			Binding: "billing/payments-anthropic", Reason: bindings.ReasonCredentialIssued,
		}, http.StatusForbidden},
		{"unknown reason", goodToken, bindings.Report{Binding: namespace + "/first", Reason: "Rotated"}, http.StatusBadRequest},
		{"identity of a service account", goodToken, bindings.Report{Reason: bindings.ReasonNoWorkloadIdentity}, http.StatusConflict},
	} {
		if got := post(t, srv.URL, tc.token, tc.rep); got != tc.want {
			t.Errorf("%s: status %d; want %d", tc.name, got, tc.want)
		}
	}

	resp := get(t, srv.URL+"/v1/reports", goodToken)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /v1/reports: status %d", resp.StatusCode)
	}

	events := rec.take()
	if got := reasons(events); len(got) != 2 || got[0] != "Normal CredentialIssued" || got[1] != "Warning TokenExchangeFailed" {
		t.Fatalf("events %v; want the issued and the long one", got)
	}

	pod, ok := events[0].regarding.(*corev1.Pod)
	if !ok || pod.Namespace != namespace || pod.Name != podName || pod.UID != podUID {
		t.Errorf("regarding %+v; want the caller's pod, with its UID", events[0].regarding)
	}

	if b, ok := events[0].related.(*v1alpha1.CredentialBinding); !ok || b.Name != "first" {
		t.Errorf("related %+v; want the binding", events[0].related)
	}

	if events[0].note != issued || !strings.HasPrefix(events[0].action, "ExchangeToken-") {
		t.Errorf("note %q, action %q", events[0].note, events[0].action)
	}

	if note := events[1].note; len(note) > 1024 || !utf8.ValidString(note) {
		t.Errorf("a long message became a %d-byte note", len(note))
	}
}

// TestReportsMessages: the events library merges events with the same
// reason and action, whatever their notes, so another message gets another
// action, and the same one the same.
func TestReportsMessages(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	srv := httptest.NewServer(&controller.API{
		Auth: fakeAuth{}, Reader: newClient(t, anthropicSource(), newBinding("first")),
		SystemNamespace: systemNamespace, Changes: controller.NewBroadcaster(), Recorder: rec,
	})
	t.Cleanup(srv.Close)

	for _, msg := range []string{"invalid_grant — AADSTS70021", "status 403: denied", "invalid_grant — AADSTS70021"} {
		rep := bindings.Report{Binding: namespace + "/first", Reason: bindings.ReasonTokenExchangeFailed, Message: msg}
		if got := post(t, srv.URL, goodToken, rep); got != http.StatusNoContent {
			t.Fatalf("status %d", got)
		}
	}

	ev := rec.take()
	if len(ev) != 3 || ev[0].action == ev[1].action || ev[0].action != ev[2].action {
		t.Errorf("events %+v; want the second's action to differ", ev)
	}
}

// revocableAuth stops accepting goodToken once revoked, as when its pod goes.
type revocableAuth struct {
	fakeAuth

	revoked atomic.Bool
}

func (a *revocableAuth) Authenticate(ctx context.Context, token string) (controller.Caller, error) {
	if a.revoked.Load() {
		return controller.Caller{}, errBadToken
	}

	return a.fakeAuth.Authenticate(ctx, token)
}

// unavailableAuth can't check tokens, as when the Kubernetes API is down.
type unavailableAuth struct{}

func (unavailableAuth) Authenticate(context.Context, string) (controller.Caller, error) {
	return controller.Caller{}, fmt.Errorf("%w: TokenReview: etcd timeout", controller.ErrAuthUnavailable)
}

// TestReportsAuthUnavailable: a report whose token can't be checked now gets
// a 503, so the proxy tries again, rather than a 403 it gives up on.
func TestReportsAuthUnavailable(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(&controller.API{
		Auth: unavailableAuth{}, Reader: newClient(t, anthropicSource(), newBinding("first")),
		SystemNamespace: systemNamespace, Changes: controller.NewBroadcaster(), Recorder: &recorder{},
	})
	t.Cleanup(srv.Close)

	if got := post(t, srv.URL, goodToken, bindings.Report{
		Binding: namespace + "/first", Reason: bindings.ReasonCredentialIssued,
	}); got != http.StatusServiceUnavailable {
		t.Errorf("status %d; want 503", got)
	}
}

// countingReader counts lists.
type countingReader struct {
	client.Reader

	lists atomic.Int32
}

func (r *countingReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	r.lists.Add(1)

	return r.Reader.List(ctx, list, opts...)
}

// TestReportsOnAWatch: a report with the token of an open watch takes the
// bindings that watch sent, without listing them again, but its token is
// still checked.
func TestReportsOnAWatch(t *testing.T) {
	t.Parallel()

	auth := &revocableAuth{}
	reader := &countingReader{Reader: newClient(t, anthropicSource(), newBinding("first"))}
	srv := httptest.NewServer(&controller.API{
		Auth: auth, Reader: reader,
		SystemNamespace: systemNamespace, Changes: controller.NewBroadcaster(), Recorder: &recorder{},
	})
	t.Cleanup(srv.Close)

	watch := get(t, srv.URL+"/v1/bindings?watch=true", goodToken)
	t.Cleanup(func() { _ = watch.Body.Close() })
	next(t, bufio.NewScanner(watch.Body))

	lists := reader.lists.Load()

	if got := post(t, srv.URL, goodToken, bindings.Report{
		Binding: namespace + "/first", Reason: bindings.ReasonCredentialIssued,
	}); got != http.StatusNoContent {
		t.Errorf("status %d", got)
	}

	if got := post(t, srv.URL, goodToken, bindings.Report{
		Binding: "billing/payments-anthropic", Reason: bindings.ReasonCredentialIssued,
	}); got != http.StatusForbidden {
		t.Errorf("another pod's binding: status %d", got)
	}

	if n := reader.lists.Load(); n != lists {
		t.Errorf("%d lists for two reports; want none", n-lists)
	}

	auth.revoked.Store(true)

	if got := post(t, srv.URL, goodToken, bindings.Report{
		Binding: namespace + "/first", Reason: bindings.ReasonCredentialIssued,
	}); got != http.StatusForbidden {
		t.Errorf("a revoked token: status %d", got)
	}
}

// TestReportsSPIRE: the controller says why a pod has no identity, whatever
// the proxy reports.
func TestReportsSPIRE(t *testing.T) {
	t.Parallel()

	c := twoIdentities(t)
	rec := &recorder{}
	report := func(auth controller.Authenticator) int {
		srv := httptest.NewServer(&controller.API{
			Auth: auth, Reader: c, SystemNamespace: systemNamespace,
			Changes: controller.NewBroadcaster(), TrustDomain: trustDomain, Recorder: rec,
		})
		defer srv.Close()

		return post(t, srv.URL, goodToken, bindings.Report{Reason: bindings.ReasonNoWorkloadIdentity, Message: "the proxy's words"})
	}

	// Both identities match, and neither is more specific.
	if got := report(labeledAuth{"team": serviceAccount}); got != http.StatusNoContent {
		t.Errorf("ambiguous: status %d", got)
	}

	if got := report(labeledAuth(nil)); got != http.StatusConflict {
		t.Errorf("with an identity: status %d; want 409", got)
	}

	events := rec.take()
	if len(events) != 1 || events[0].typ != corev1.EventTypeWarning || events[0].note == "" || events[0].note == "the proxy's words" {
		t.Errorf("events %+v; want one warning, with the controller's reason", events)
	}
}

// TestReportsSlowBody: a report whose body never finishes doesn't hold the
// connection, while a watch on another connection keeps streaming.
func TestReportsSlowBody(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(&controller.API{
		Auth: fakeAuth{}, Reader: newClient(t, anthropicSource(), newBinding("first")),
		SystemNamespace: systemNamespace, Changes: controller.NewBroadcaster(), Recorder: &recorder{},
		ReportTimeout: 100 * time.Millisecond,
	})
	t.Cleanup(srv.Close)

	watch := get(t, srv.URL+"/v1/bindings?watch=true", goodToken)
	t.Cleanup(func() { _ = watch.Body.Close() })

	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Promise 100 bytes, send one.
	_, err = io.WriteString(conn, "POST /v1/reports HTTP/1.1\r\nHost: waterwheel\r\nAuthorization: Bearer "+goodToken+
		"\r\nContent-Length: 100\r\n\r\n{")
	if err != nil {
		t.Fatal(err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no answer to a stalled report: %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d; want 400", resp.StatusCode)
	}

	time.Sleep(200 * time.Millisecond)

	lines := bufio.NewScanner(watch.Body)
	if n := next(t, lines); n != 1 {
		t.Errorf("the watch got %d bindings; want 1", n)
	}
}
