// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"go.riptides.io/waterwheel/api/v1alpha1"
)

// reasonRecorder keeps its events' types and reasons.
type reasonRecorder []string

func (r *reasonRecorder) Eventf(_, _ runtime.Object, typ, reason, _, _ string, _ ...any) {
	*r = append(*r, typ+" "+reason)
}

// TestShadowedEvents: being shadowed is a warning, and its end an event too.
func TestShadowedEvents(t *testing.T) {
	t.Parallel()

	ready := metav1.Condition{Type: ConditionReady, Status: metav1.ConditionTrue, Reason: "Registered"}
	shadowed := metav1.Condition{Type: ConditionShadowed, Status: metav1.ConditionTrue, Reason: "MoreSpecificIdentity"}

	var rec reasonRecorder

	w := &v1alpha1.WorkloadIdentity{}
	recordTransitions(&rec, w, actionRegister, []metav1.Condition{ready}, []metav1.Condition{ready, shadowed})
	recordTransitions(&rec, w, actionRegister, []metav1.Condition{ready, shadowed}, []metav1.Condition{ready})

	if want := []string{"Warning MoreSpecificIdentity", "Normal NotShadowed"}; !slices.Equal(rec, want) {
		t.Errorf("events %v; want %v", rec, want)
	}
}
