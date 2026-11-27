// SPDX-License-Identifier: Apache-2.0

package controller_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
		name      string
		status    func(s *authnv1.TokenReviewStatus)
		pod       func(p *corev1.Pod)
		createErr error // the TokenReview's
		getErr    error // the pod's
		ok        bool
		// unavailable: the token couldn't be checked, rather than failed.
		unavailable bool
	}{
		{name: "valid", ok: true},
		{name: "not authenticated", status: func(s *authnv1.TokenReviewStatus) { s.Authenticated = false }},
		{name: "other audience", status: func(s *authnv1.TokenReviewStatus) { s.Audiences = []string{"https://api.anthropic.com"} }},
		{name: "not a service account", status: func(s *authnv1.TokenReviewStatus) { s.User.Username = "alice" }},
		{name: "not pod-bound", status: func(s *authnv1.TokenReviewStatus) { s.User.Extra = nil }},
		{name: "pod replaced", pod: func(p *corev1.Pod) { p.UID = "uid-2" }},
		{name: "pod runs as another account", pod: func(p *corev1.Pod) { p.Spec.ServiceAccountName = "other" }},
		{name: "pod finished", pod: func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded }},
		{name: "pod gone", getErr: apierrors.NewNotFound(corev1.Resource("pods"), podName)},
		{name: "TokenReview fails", createErr: apierrors.NewServiceUnavailable("etcd timeout"), unavailable: true},
		{name: "pod read fails", getErr: apierrors.NewTimeoutError("pods", 1), unavailable: true},
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

					return tt.createErr
				},
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if tt.getErr != nil {
						return tt.getErr
					}

					return c.Get(ctx, key, obj, opts...)
				},
			}).Build()

			token := jwt(t, []string{controller.Audience}, time.Now().Add(time.Hour))

			caller, err := (&controller.TokenReviewer{Client: c, Pods: c}).Authenticate(t.Context(), token)
			if tt.ok != (err == nil) {
				t.Fatalf("Authenticate() error = %v; want ok = %v", err, tt.ok)
			}

			if got := errors.Is(err, controller.ErrAuthUnavailable); got != tt.unavailable {
				t.Errorf("Authenticate() error = %v; want unavailable = %v", err, tt.unavailable)
			}

			if tt.ok && caller.Identity() != identity {
				t.Errorf("Identity() = %q", caller.Identity())
			}
		})
	}
}

// jwt is an unsigned token with an audience and an expiry, each left out when
// nil or zero; the TokenReview in these tests is a fake.
func jwt(t *testing.T, audience any, expiry time.Time) string {
	t.Helper()

	claims := map[string]any{}
	if audience != nil {
		claims["aud"] = audience
	}

	if !expiry.IsZero() {
		claims["exp"] = expiry.Unix()
	}

	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}

	enc := base64.RawURLEncoding.EncodeToString

	return enc([]byte(`{"alg":"RS256"}`)) + "." + enc(payload) + ".c2ln"
}

// What can't pass a TokenReview doesn't cost the API server one.
func TestTokenReviewerSkipsImplausibleTokens(t *testing.T) {
	t.Parallel()

	soon := time.Now().Add(time.Hour)

	tokens := map[string]string{
		"not a JWT":             "hunter2",
		"not base64":            "a.!.c",
		"not JSON":              "a.YWJj.c",
		"another audience":      jwt(t, []string{"https://api.anthropic.com"}, soon),
		"no audience":           jwt(t, nil, soon),
		"audience not a string": jwt(t, 7, soon),
		"expired":               jwt(t, controller.Audience, time.Now().Add(-time.Hour)),
		"no expiry":             jwt(t, controller.Audience, time.Time{}),
	}

	for name, token := range tokens {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			c := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
				Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
					t.Error("a TokenReview was created")

					return nil
				},
			}).Build()

			_, err := (&controller.TokenReviewer{Client: c, Pods: c}).Authenticate(t.Context(), token)
			if err == nil || errors.Is(err, controller.ErrAuthUnavailable) {
				t.Errorf("Authenticate() error = %v; want the token refused", err)
			}
		})
	}
}
