// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"hash/fnv"
	"io"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"

	"go.riptides.io/waterwheel/internal/bindings"
)

// Event actions.
const (
	actionReconcile       = "Reconcile"
	actionRegister        = "Register"
	actionExchangeToken   = "ExchangeToken"
	actionResolveIdentity = "ResolveIdentity"
)

// warnWhenTrue are the conditions whose True is the bad state.
var warnWhenTrue = map[string]bool{ConditionShadowed: true}

// cleared are the events for conditions whose removal ends a warning.
var cleared = map[string]struct{ reason, note string }{
	ConditionShadowed: {"NotShadowed", "no more specific WorkloadIdentity matches its pods any more"},
}

// recordTransitions records an event for each condition that is new, or whose
// status or reason differs from before, and for a Shadowed condition that went
// away. An event is a Warning when the condition is in its bad state.
func recordTransitions(rec events.EventRecorder, obj runtime.Object, action string, before, after []metav1.Condition) {
	if rec == nil {
		return
	}

	for _, c := range after {
		if old := meta.FindStatusCondition(before, c.Type); old != nil && old.Status == c.Status && old.Reason == c.Reason {
			continue
		}

		typ := corev1.EventTypeNormal
		if (c.Status == metav1.ConditionTrue) == warnWhenTrue[c.Type] {
			typ = corev1.EventTypeWarning
		}

		eventf(rec, obj, nil, typ, c.Reason, action, c.Message)
	}

	for _, c := range before {
		if ev, ok := cleared[c.Type]; ok && meta.FindStatusCondition(after, c.Type) == nil {
			eventf(rec, obj, nil, corev1.EventTypeNormal, ev.reason, action, ev.note)
		}
	}
}

// eventf records an event. The events library merges an event into an earlier
// one about the same objects with the same reason and action, whatever its
// note, so the action carries a hash of the note: another message makes
// another event, and the same one a repeat.
func eventf(rec events.EventRecorder, regarding, related runtime.Object, typ, reason, action, note string) {
	note = bindings.Truncate(note)

	h := fnv.New32a()
	_, _ = io.WriteString(h, note)

	rec.Eventf(regarding, related, typ, reason, fmt.Sprintf("%s-%08x", action, h.Sum32()), "%s", note)
}
