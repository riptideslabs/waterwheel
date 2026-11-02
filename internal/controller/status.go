// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"go.riptides.io/waterwheel/api/v1alpha1"
	"go.riptides.io/waterwheel/internal/bindings"
)

// Binding conditions.
const (
	// ConditionReady says whether proxies get a CredentialBinding.
	ConditionReady = "Ready"
	// ConditionPodsUpToDate says whether every injected pod has the projected
	// token audience the binding needs. Audiences are fixed at admission, so
	// pods started before the binding needed a new one must restart.
	ConditionPodsUpToDate = "PodsUpToDate"
)

// BindingStatus keeps each CredentialBinding's conditions.
type BindingStatus struct {
	Client client.Client
	// Pods lists pods; an uncached reader, so the controller doesn't cache every pod.
	Pods            client.Reader
	SystemNamespace string
	// ServiceAccountIDsOnly is set with the serviceAccountToken identity
	// source: a binding applies only to a workloadID ns/<ns>/sa/<sa>.
	ServiceAccountIDsOnly bool
}

func (r *BindingStatus) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.CredentialBinding{}).
		Watches(&v1alpha1.CredentialSource{}, handler.EnqueueRequestsFromMapFunc(r.bindingsOfSource)).
		Complete(r)
}

func (r *BindingStatus) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var b v1alpha1.CredentialBinding
	if err := r.Client.Get(ctx, req.NamespacedName, &b); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	ready, audience := r.ready(ctx, &b)
	changed := setCondition(&b, ready)

	var result ctrl.Result

	if ready.Status == metav1.ConditionTrue && r.ServiceAccountIDsOnly {
		pods := r.podsUpToDate(ctx, &b, audience)
		changed = setCondition(&b, pods) || changed

		// Pod restarts don't trigger a reconcile; look again later.
		if pods.Status != metav1.ConditionTrue {
			result.RequeueAfter = time.Minute
		}
	} else if meta.RemoveStatusCondition(&b.Status.Conditions, ConditionPodsUpToDate) {
		changed = true
	}

	if !changed {
		return result, nil
	}

	return result, r.Client.Status().Update(ctx, &b)
}

func setCondition(b *v1alpha1.CredentialBinding, c metav1.Condition) bool {
	c.ObservedGeneration = b.Generation

	return meta.SetStatusCondition(&b.Status.Conditions, c)
}

// ready returns the Ready condition and, when ready, the subject token audience.
func (r *BindingStatus) ready(ctx context.Context, b *v1alpha1.CredentialBinding) (metav1.Condition, string) {
	notReady := func(reason, msg string) metav1.Condition {
		return metav1.Condition{Type: ConditionReady, Status: metav1.ConditionFalse, Reason: reason, Message: msg}
	}

	if err := bindings.Check(b, r.SystemNamespace); err != nil {
		return notReady("ForeignWorkloadID", err.Error()), ""
	}

	if _, _, ok := bindings.ParseServiceAccountID(b.Spec.WorkloadID); r.ServiceAccountIDsOnly && !ok {
		return notReady("NotAServiceAccount",
			"with service account tokens, workloadID must be ns/<namespace>/sa/<service account>"), ""
	}

	res, err := bindings.Resolve(ctx, r.Client, b)

	switch {
	case apierrors.IsNotFound(err):
		return notReady("SourceNotFound", "CredentialSource "+b.Spec.CredentialSource+" not found"), ""
	case err != nil:
		return notReady("SourceInvalid", err.Error()), ""
	}

	return metav1.Condition{
		Type: ConditionReady, Status: metav1.ConditionTrue, Reason: "Resolved",
		Message: "subject token audience " + res.Audience,
	}, res.Audience
}

func (r *BindingStatus) podsUpToDate(ctx context.Context, b *v1alpha1.CredentialBinding, audience string) metav1.Condition {
	ns, sa, _ := bindings.ParseServiceAccountID(b.Spec.WorkloadID)

	var pods corev1.PodList
	if err := r.Pods.List(ctx, &pods, client.InNamespace(ns), client.MatchingFields{"spec.serviceAccountName": sa}); err != nil {
		return metav1.Condition{Type: ConditionPodsUpToDate, Status: metav1.ConditionUnknown, Reason: "ListFailed", Message: err.Error()}
	}

	var stale []string

	for _, p := range pods.Items {
		if p.Annotations[AnnotationInjected] != strTrue || p.DeletionTimestamp != nil ||
			p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}

		if !slices.Contains(strings.Split(p.Annotations[AnnotationAudiences], ","), audience) {
			stale = append(stale, p.Name)
		}
	}

	if len(stale) == 0 {
		return metav1.Condition{
			Type: ConditionPodsUpToDate, Status: metav1.ConditionTrue, Reason: "UpToDate",
			Message: "every injected pod of " + sa + " has a token for " + audience,
		}
	}

	slices.Sort(stale)

	shown := stale[:min(len(stale), 5)]

	return metav1.Condition{
		Type: ConditionPodsUpToDate, Status: metav1.ConditionFalse, Reason: "RestartRequired",
		Message: fmt.Sprintf("%d pod(s) started before the binding needed a token for %s; restart them: %s",
			len(stale), audience, strings.Join(shown, ", ")),
	}
}

// bindingsOfSource maps a CredentialSource to the bindings that use it.
func (r *BindingStatus) bindingsOfSource(ctx context.Context, src client.Object) []reconcile.Request {
	var list v1alpha1.CredentialBindingList
	if err := r.Client.List(ctx, &list, client.InNamespace(src.GetNamespace())); err != nil {
		return nil
	}

	var reqs []reconcile.Request

	for _, b := range list.Items {
		if b.Spec.CredentialSource == src.GetName() {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&b)})
		}
	}

	return reqs
}
