// SPDX-License-Identifier: Apache-2.0

package bindings_test

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"go.riptides.io/waterwheel/api/v1alpha1"
	"go.riptides.io/waterwheel/internal/bindings"
)

const (
	system = "waterwheel-system"
	stsURL = "https://sts.example/token"
)

func TestAudience(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		spec v1alpha1.CredentialSourceSpec
		want string
	}{
		{"anthropic default", v1alpha1.CredentialSourceSpec{Anthropic: &v1alpha1.AnthropicWIFCredentialProvider{}}, "https://api.anthropic.com"},
		{"anthropic listed", v1alpha1.CredentialSourceSpec{Anthropic: &v1alpha1.AnthropicWIFCredentialProvider{IDTokenAudience: []string{"a", "b"}}}, "a"},
		{"openai default", v1alpha1.CredentialSourceSpec{OpenAI: &v1alpha1.OpenAIWIFCredentialProvider{}}, "https://api.openai.com"},
		{
			"gcp default",
			v1alpha1.CredentialSourceSpec{GCP: &v1alpha1.GCPWIFCredentialProvider{OIDCProviderID: "//iam.googleapis.com/projects/1/x"}},
			"https://iam.googleapis.com/projects/1/x",
		},
		{"azure default", v1alpha1.CredentialSourceSpec{Azure: &v1alpha1.EntraWIFCredentialProvider{}}, "api://AzureADTokenExchange"},
		{"rfc8693 default", v1alpha1.CredentialSourceSpec{RFC8693: &v1alpha1.RFC8693CredentialProvider{TokenEndpointURL: stsURL}}, stsURL},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := bindings.Audience(tt.spec)
			if err != nil || got != tt.want {
				t.Errorf("Audience() = %q, %v; want %q", got, err, tt.want)
			}
		})
	}

	if _, err := bindings.Audience(v1alpha1.CredentialSourceSpec{}); err == nil {
		t.Error("Audience() of an empty source: want an error")
	}
}

func TestFor(t *testing.T) {
	t.Parallel()

	id := bindings.ServiceAccountID("support", "bot")
	c := newClient(t,
		source("support", "anthropic", v1alpha1.CredentialSourceSpec{Anthropic: &v1alpha1.AnthropicWIFCredentialProvider{}}),
		source("support", "sts", v1alpha1.CredentialSourceSpec{RFC8693: &v1alpha1.RFC8693CredentialProvider{TokenEndpointURL: stsURL}}),
		source(system, "shared", v1alpha1.CredentialSourceSpec{OpenAI: &v1alpha1.OpenAIWIFCredentialProvider{}}),
		binding("support", "b-anthropic", id, "anthropic", v1alpha1.HTTPHeaderInjection{
			Destinations: []v1alpha1.Destination{{Host: "api.anthropic.com"}},
		}),
		binding("support", "a-sts", id, "sts", v1alpha1.HTTPHeaderInjection{
			Destinations: []v1alpha1.Destination{{Host: "*.example.com", Port: 8443}},
			Header:       "X-Token",
			Scheme:       "-",
		}),
		binding("support", "other-sa", bindings.ServiceAccountID("support", "other"), "anthropic", inj("api.anthropic.com")),
		binding("support", "no-source", id, "missing", inj("api.anthropic.com")),
		// A binding in another namespace naming this identity: the policy
		// rejects it, and if it got in anyway, it must not count.
		source("billing", "anthropic", v1alpha1.CredentialSourceSpec{Anthropic: &v1alpha1.AnthropicWIFCredentialProvider{}}),
		binding("billing", "foreign", id, "anthropic", inj("api.anthropic.com")),
		binding(system, "shared", id, "shared", inj("api.openai.com")),
	)

	snap, err := bindings.For(t.Context(), c, system, "support", id)
	if err != nil {
		t.Fatal(err)
	}

	got := make([]string, 0, len(snap.Bindings))
	for _, b := range snap.Bindings {
		got = append(got, b.Namespace+"/"+b.Name)
	}

	want := []string{"support/a-sts", "support/b-anthropic", system + "/shared"}
	if len(got) != len(want) {
		t.Fatalf("bindings = %v; want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("bindings = %v; want %v", got, want)
		}
	}

	sts, anthropic := snap.Bindings[0], snap.Bindings[1]
	if sts.Header != "X-Token" || sts.Scheme != "" || sts.Destinations[0].Port != 8443 || sts.Audience != stsURL {
		t.Errorf("a-sts = %+v", sts)
	}

	if anthropic.Header != "Authorization" || anthropic.Scheme != "Bearer" || anthropic.Destinations[0].Port != 443 {
		t.Errorf("b-anthropic = %+v", anthropic)
	}

	auds := snap.Audiences()
	if len(auds) != 3 || auds[0] != "https://api.anthropic.com" {
		t.Errorf("Audiences() = %v", auds)
	}
}

func TestCheck(t *testing.T) {
	t.Parallel()

	if err := bindings.Check(binding("support", "x", "ns/billing/sa/x", "s", inj("a.example.com")), system); err == nil {
		t.Error("a foreign workloadID passed Check")
	}

	if err := bindings.Check(binding(system, "x", "ns/billing/sa/x", "s", inj("a.example.com")), system); err != nil {
		t.Errorf("the system namespace may use any ID: %v", err)
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

func source(ns, name string, spec v1alpha1.CredentialSourceSpec) *v1alpha1.CredentialSource {
	return &v1alpha1.CredentialSource{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: spec}
}

func binding(ns, name, workloadID, src string, injection v1alpha1.HTTPHeaderInjection) *v1alpha1.CredentialBinding {
	return &v1alpha1.CredentialBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: v1alpha1.CredentialBindingSpec{
			WorkloadID:       workloadID,
			CredentialSource: src,
			Propagation:      v1alpha1.CredentialBindingPropagation{Injection: injection},
		},
	}
}

func inj(host string) v1alpha1.HTTPHeaderInjection {
	return v1alpha1.HTTPHeaderInjection{Destinations: []v1alpha1.Destination{{Host: host}}}
}

func TestParseServiceAccountID(t *testing.T) {
	t.Parallel()

	if ns, sa, ok := bindings.ParseServiceAccountID("ns/support/sa/bot"); !ok || ns != "support" || sa != "bot" {
		t.Errorf("ParseServiceAccountID() = %q, %q, %v", ns, sa, ok)
	}

	for _, id := range []string{"ns/support/app/worker", "ns//sa/bot", "ns/support/sa/", "ns/a/b/sa/c", "spiffe://x/ns/a/sa/b"} {
		if _, _, ok := bindings.ParseServiceAccountID(id); ok {
			t.Errorf("ParseServiceAccountID(%q) accepted a non-service-account ID", id)
		}
	}

	a, b := bindings.TokenFile("https://api.anthropic.com"), bindings.TokenFile("waterwheel-controller")
	if a == b || a != "aud-"+a[len("aud-"):] || len(a) != len("aud-")+16 {
		t.Errorf("TokenFile() = %q, %q; want distinct aud-<16 hex> names", a, b)
	}
}
