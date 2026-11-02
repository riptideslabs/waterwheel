// SPDX-License-Identifier: Apache-2.0

package controller_test

import (
	"context"
	"testing"

	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"go.riptides.io/waterwheel/internal/controller"
)

func TestTokenReviewer(t *testing.T) {
	t.Parallel()

	ok := authnv1.TokenReviewStatus{
		Authenticated: true,
		Audiences:     []string{controller.Audience},
		User: authnv1.UserInfo{
			Username: "system:serviceaccount:support:bot",
			Extra: map[string]authnv1.ExtraValue{
				"authentication.kubernetes.io/pod-name": {podName},
				"authentication.kubernetes.io/pod-uid":  {"uid-1"},
			},
		},
	}

	tests := []struct {
		name   string
		status func(s *authnv1.TokenReviewStatus)
		pod    func(p *corev1.Pod)
		ok     bool
	}{
		{name: "valid", ok: true},
		{name: "not authenticated", status: func(s *authnv1.TokenReviewStatus) { s.Authenticated = false }},
		{name: "other audience", status: func(s *authnv1.TokenReviewStatus) { s.Audiences = []string{"https://api.anthropic.com"} }},
		{name: "not a service account", status: func(s *authnv1.TokenReviewStatus) { s.User.Username = "alice" }},
		{name: "not pod-bound", status: func(s *authnv1.TokenReviewStatus) { s.User.Extra = nil }},
		{name: "pod replaced", pod: func(p *corev1.Pod) { p.UID = "uid-2" }},
		{name: "pod runs as another account", pod: func(p *corev1.Pod) { p.Spec.ServiceAccountName = "other" }},
		{name: "pod finished", pod: func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			status := *ok.DeepCopy()
			if tt.status != nil {
				tt.status(&status)
			}

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: podName, UID: "uid-1"},
				Spec:       corev1.PodSpec{ServiceAccountName: serviceAccount},
				Status:     corev1.PodStatus{Phase: corev1.PodRunning},
			}
			if tt.pod != nil {
				tt.pod(pod)
			}

			c := fake.NewClientBuilder().WithObjects(pod).WithInterceptorFuncs(interceptor.Funcs{
				Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
					review, ok := obj.(*authnv1.TokenReview)
					if !ok {
						t.Fatalf("unexpected Create of %T", obj)
					}

					review.Status = status

					return nil
				},
			}).Build()

			caller, err := (&controller.TokenReviewer{Client: c, Pods: c}).Authenticate(t.Context(), "token")
			if tt.ok != (err == nil) {
				t.Fatalf("Authenticate() error = %v; want ok = %v", err, tt.ok)
			}

			if tt.ok && caller.Identity() != identity {
				t.Errorf("Identity() = %q", caller.Identity())
			}
		})
	}
}
