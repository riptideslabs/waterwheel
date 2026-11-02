// SPDX-License-Identifier: Apache-2.0

package controller_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"go.riptides.io/waterwheel/api/v1alpha1"
	"go.riptides.io/waterwheel/internal/controller"
)

func TestBindingStatus(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	pod := func(name, audiences string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Annotations: map[string]string{
				controller.AnnotationInjected: "true", controller.AnnotationAudiences: audiences,
			}},
			Spec:   corev1.PodSpec{ServiceAccountName: serviceAccount},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
	}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.CredentialBinding{}).
		WithIndex(&corev1.Pod{}, "spec.serviceAccountName", func(o client.Object) []string {
			if p, ok := o.(*corev1.Pod); ok {
				return []string{p.Spec.ServiceAccountName}
			}

			return nil
		}).
		WithObjects(
			&v1alpha1.CredentialSource{
				ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: sourceName},
				Spec:       v1alpha1.CredentialSourceSpec{Anthropic: &v1alpha1.AnthropicWIFCredentialProvider{}},
			},
			newBinding("bot-anthropic"),
			pod("bot-old", ""),
			pod("bot-new", anthropicAudience),
		).Build()

	r := &controller.BindingStatus{Client: c, Pods: c, SystemNamespace: systemNamespace, ServiceAccountIDsOnly: true}
	key := client.ObjectKey{Namespace: namespace, Name: "bot-anthropic"}

	conditions := func() (*metav1.Condition, *metav1.Condition) {
		t.Helper()

		res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
		if err != nil {
			t.Fatal(err)
		}

		var b v1alpha1.CredentialBinding
		if err := c.Get(t.Context(), key, &b); err != nil {
			t.Fatal(err)
		}

		pods := meta.FindStatusCondition(b.Status.Conditions, controller.ConditionPodsUpToDate)
		if pods != nil && pods.Status != metav1.ConditionTrue && res.RequeueAfter == 0 {
			t.Error("stale pods but no requeue")
		}

		return meta.FindStatusCondition(b.Status.Conditions, controller.ConditionReady), pods
	}

	ready, pods := conditions()
	if ready == nil || ready.Status != metav1.ConditionTrue {
		t.Fatalf("Ready = %+v", ready)
	}

	if pods == nil || pods.Reason != "RestartRequired" {
		t.Fatalf("PodsUpToDate = %+v; want RestartRequired for bot-old", pods)
	}

	// The old pod restarts and gets the audience.
	if err := c.Delete(t.Context(), pod("bot-old", "")); err != nil {
		t.Fatal(err)
	}

	if _, pods := conditions(); pods.Status != metav1.ConditionTrue {
		t.Errorf("PodsUpToDate = %+v after the restart", pods)
	}
}
