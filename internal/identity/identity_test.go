// SPDX-License-Identifier: Apache-2.0

package identity_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	"go.riptides.io/waterwheel/api/v1alpha1"
	"go.riptides.io/waterwheel/internal/identity"
)

const (
	system = "waterwheel-system"
	sa     = "bot"
	ns     = "support"
	canary = "canary"
)

var epoch = time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)

// wid is a WorkloadIdentity created age minutes after epoch.
func wid(ns, name, id string, age int, entries ...map[string]string) v1alpha1.WorkloadIdentity {
	w := v1alpha1.WorkloadIdentity{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: name,
			CreationTimestamp: metav1.NewTime(epoch.Add(time.Duration(age) * time.Minute)),
		},
		Spec: v1alpha1.WorkloadIdentitySpec{WorkloadID: id},
	}

	for _, e := range entries {
		sel := v1alpha1.WorkloadSelector{}
		for k, v := range e {
			sel[k] = intstr.FromString(v)
		}

		w.Spec.Selectors = append(w.Spec.Selectors, sel)
	}

	return w
}

func bot(labels map[string]string) identity.Pod {
	return identity.Pod{Namespace: ns, Name: "bot-1", ServiceAccount: sa, Labels: labels}
}

func resolve(t *testing.T, s *identity.Set, p identity.Pod) string {
	t.Helper()

	id, err := s.Resolve(p)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	return id
}

func TestMostSpecificWins(t *testing.T) {
	t.Parallel()

	s := identity.Build([]v1alpha1.WorkloadIdentity{
		wid(ns, "general", "ns/support/general", 1, map[string]string{identity.KeyServiceAccount: sa}),
		wid(ns, "canary", "ns/support/canary", 2,
			map[string]string{identity.KeyServiceAccount: sa, "k8s:label:track": canary}),
	}, system)

	if got := resolve(t, s, bot(nil)); got != "ns/support/general" {
		t.Errorf("without the label: %s", got)
	}

	if got := resolve(t, s, bot(map[string]string{"track": canary})); got != "ns/support/canary" {
		t.Errorf("with the label: %s", got)
	}

	want := []types.NamespacedName{{Namespace: ns, Name: "canary"}}
	if got := s.Shadowed[types.NamespacedName{Namespace: ns, Name: "general"}]; !slices.Equal(got, want) {
		t.Errorf("general shadowed by %v, want %v", got, want)
	}

	if len(s.Problems) != 0 {
		t.Errorf("problems: %v", s.Problems)
	}
}

func TestAmbiguous(t *testing.T) {
	t.Parallel()

	s := identity.Build([]v1alpha1.WorkloadIdentity{
		wid(ns, "by-sa", "ns/support/a", 1, map[string]string{identity.KeyServiceAccount: sa}),
		wid(ns, "by-label", "ns/support/b", 2, map[string]string{"k8s:label:app": sa}),
	}, system)

	if _, err := s.Resolve(bot(map[string]string{"app": sa})); !errors.Is(err, identity.ErrAmbiguous) {
		t.Errorf("err = %v, want ErrAmbiguous", err)
	}

	if got := resolve(t, s, bot(nil)); got != "ns/support/a" {
		t.Errorf("only the service account matches: %s", got)
	}

	if _, err := s.Resolve(identity.Pod{Namespace: ns, ServiceAccount: "other"}); !errors.Is(err, identity.ErrNoIdentity) {
		t.Errorf("err = %v, want ErrNoIdentity", err)
	}
}

func TestSameSelectorsOlderWins(t *testing.T) {
	t.Parallel()

	sel := map[string]string{identity.KeyServiceAccount: sa}
	newer := types.NamespacedName{Namespace: ns, Name: "newer"}

	// The order of the list doesn't matter; creation time does.
	s := identity.Build([]v1alpha1.WorkloadIdentity{
		wid(ns, "newer", "ns/support/newer", 2, sel),
		wid(ns, "older", "ns/support/older", 1, sel),
		wid(ns, "same-id", "ns/support/older", 3, sel),
	}, system)

	if got := resolve(t, s, bot(nil)); got != "ns/support/older" {
		t.Errorf("resolved %s", got)
	}

	if p := s.Problems[newer]; p.Reason != identity.ReasonSelectorsOverlap {
		t.Errorf("newer: %+v", p)
	}

	if _, ok := s.Problems[types.NamespacedName{Namespace: ns, Name: "same-id"}]; ok {
		t.Error("the same selectors with the same workload ID don't conflict")
	}
}

func TestNamespaces(t *testing.T) {
	t.Parallel()

	s := identity.Build([]v1alpha1.WorkloadIdentity{
		wid("other", sa, "ns/other/bot", 1, map[string]string{identity.KeyServiceAccount: sa}),
		wid("other", "foreign-id", "ns/support/bot", 1, map[string]string{identity.KeyServiceAccount: "x"}),
		wid("other", "foreign-ns", "ns/other/x", 1,
			map[string]string{identity.KeyNamespace: ns, identity.KeyServiceAccount: "x"}),
		wid(system, "reserved", "waterwheel/agent", 1, map[string]string{identity.KeyServiceAccount: "x"}),
		wid(system, "shared", "shared/reporter", 1, map[string]string{"k8s:label:role": "reporter"}),
	}, system)

	if _, err := s.Resolve(bot(nil)); !errors.Is(err, identity.ErrNoIdentity) {
		t.Errorf("an identity in another namespace applied: %v", err)
	}

	if got := resolve(t, s, bot(map[string]string{"role": "reporter"})); got != "shared/reporter" {
		t.Errorf("the system namespace's identity: %s", got)
	}

	for name, reason := range map[string]string{
		"other/foreign-id":           identity.ReasonForeignWorkloadID,
		"other/foreign-ns":           identity.ReasonForeignNamespace,
		system + "/reserved":         identity.ReasonReservedWorkloadID,
		"other/bot":                  "",
		system + "/shared":           "",
		"support/does-not-exist-yet": "",
	} {
		ns, n, _ := strings.Cut(name, "/")
		if got := s.Problems[types.NamespacedName{Namespace: ns, Name: n}].Reason; got != reason {
			t.Errorf("%s: reason %q, want %q", name, got, reason)
		}
	}

	if !s.Has(ns, "shared/reporter") || !s.Has("other", "ns/other/bot") || s.Has(ns, "ns/other/bot") ||
		!s.Has(system, "ns/other/bot") {
		t.Error("Has")
	}
}

func TestSelectors(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		key, value, reason string
	}{
		{"k8s:container:name", "app", identity.ReasonUnsupportedSelector},
		{"process:uid", "1000", identity.ReasonUnsupportedSelector},
		{"k8s:label:app", "not a label value", identity.ReasonInvalidSelector},
		{"k8s:label:-bad", "x", identity.ReasonInvalidSelector},
		{"k8s:label:app.kubernetes.io/name", sa, ""},
		{identity.KeyPodName, "bot-1", ""},
	} {
		s := identity.Build([]v1alpha1.WorkloadIdentity{
			wid(ns, "w", "ns/support/w", 1, map[string]string{tc.key: tc.value}),
		}, system)

		if got := s.Problems[types.NamespacedName{Namespace: ns, Name: "w"}].Reason; got != tc.reason {
			t.Errorf("%s=%s: reason %q, want %q", tc.key, tc.value, got, tc.reason)
		}
	}
}
