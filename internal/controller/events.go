// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
)

// Event actions.
const (
	actionReconcile       = "Reconcile"
	actionRegister        = "Register"
	actionExchangeToken   = "ExchangeToken"
	actionResolveIdentity = "ResolveIdentity"
)

// maxNote is the longest note an events.k8s.io Event takes.
const maxNote = 1024

// recordTransitions records an event for each condition that is new, or whose
// status or reason differs from before: a Warning unless it is True.
func recordTransitions(rec events.EventRecorder, obj runtime.Object, action string, before, after []metav1.Condition) {
	if rec == nil {
		return
	}

	for _, c := range after {
		if old := meta.FindStatusCondition(before, c.Type); old != nil && old.Status == c.Status && old.Reason == c.Reason {
			continue
		}

		typ := corev1.EventTypeNormal
		if c.Status != metav1.ConditionTrue {
			typ = corev1.EventTypeWarning
		}

		rec.Eventf(obj, nil, typ, c.Reason, action, "%s", truncate(c.Message, maxNote))
	}
}

// truncate cuts s to at most n bytes, at a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}

	const more = "…"

	cut := n - len(more)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}

	return s[:cut] + more
}
