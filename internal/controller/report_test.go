// SPDX-License-Identifier: Apache-2.0

package controller_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

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

	if events[0].note != issued || events[0].action != "ExchangeToken" {
		t.Errorf("note %q, action %q", events[0].note, events[0].action)
	}

	if note := events[1].note; len(note) > 1024 || !utf8.ValidString(note) {
		t.Errorf("a long message became a %d-byte note", len(note))
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
