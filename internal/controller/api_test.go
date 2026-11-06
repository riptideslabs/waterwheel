// SPDX-License-Identifier: Apache-2.0

package controller_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"go.riptides.io/waterwheel/api/v1alpha1"
	"go.riptides.io/waterwheel/internal/bindings"
	"go.riptides.io/waterwheel/internal/controller"
)

const (
	namespace       = "support"
	serviceAccount  = "bot"
	podName         = "bot-1"
	identity        = "ns/support/sa/bot"
	sourceName      = "anthropic"
	systemNamespace = "waterwheel-system"
	goodToken       = "good"
)

var errBadToken = errors.New("bad token")

type fakeAuth struct{}

func (fakeAuth) Authenticate(_ context.Context, token string) (controller.Caller, error) {
	if token != goodToken {
		return controller.Caller{}, errBadToken
	}

	return controller.Caller{Namespace: namespace, ServiceAccount: serviceAccount, Pod: podName, PodUID: podUID}, nil
}

func TestAPI(t *testing.T) {
	t.Parallel()

	c := newClient(t,
		&v1alpha1.CredentialSource{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: sourceName},
			Spec:       v1alpha1.CredentialSourceSpec{Anthropic: &v1alpha1.AnthropicWIFCredentialProvider{}},
		},
		newBinding("first"),
	)
	changes := controller.NewBroadcaster()
	srv := httptest.NewServer(&controller.API{Auth: fakeAuth{}, Reader: c, SystemNamespace: systemNamespace, Changes: changes})
	t.Cleanup(srv.Close)

	for token, want := range map[string]int{"": http.StatusUnauthorized, "bad": http.StatusForbidden, goodToken: http.StatusOK} {
		resp := get(t, srv.URL+"/v1/bindings", token)
		_ = resp.Body.Close()

		if resp.StatusCode != want {
			t.Errorf("token %q: status %d; want %d", token, resp.StatusCode, want)
		}
	}

	resp := get(t, srv.URL+"/v1/bindings?watch=true", goodToken)
	t.Cleanup(func() { _ = resp.Body.Close() })

	lines := bufio.NewScanner(resp.Body)
	if n := next(t, lines); n != 1 {
		t.Fatalf("first snapshot has %d bindings; want 1", n)
	}

	if err := c.Create(t.Context(), newBinding("second")); err != nil {
		t.Fatal(err)
	}

	changes.Notify()

	if n := next(t, lines); n != 2 {
		t.Fatalf("snapshot after the change has %d bindings; want 2", n)
	}
}

func next(t *testing.T, lines *bufio.Scanner) int {
	t.Helper()

	if !lines.Scan() {
		t.Fatalf("watch ended: %v", lines.Err())
	}

	var snap bindings.Snapshot
	if err := json.Unmarshal(lines.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}

	if snap.Identity != identity {
		t.Errorf("identity = %q", snap.Identity)
	}

	return len(snap.Bindings)
}

func get(t *testing.T, url, token string) *http.Response {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	return resp
}

func newBinding(name string) *v1alpha1.CredentialBinding {
	return &v1alpha1.CredentialBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: v1alpha1.CredentialBindingSpec{
			WorkloadID:       identity,
			CredentialSource: sourceName,
			Propagation: v1alpha1.CredentialBindingPropagation{Injection: v1alpha1.HTTPHeaderInjection{
				Destinations: []v1alpha1.Destination{{Host: "api.anthropic.com"}},
			}},
		},
	}
}

func newClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}
