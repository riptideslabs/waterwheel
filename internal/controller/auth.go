// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"go.riptides.io/waterwheel/internal/bindings"
)

// Audience is the audience of the projected token proxies present to the
// controller.
const Audience = "waterwheel-controller"

// Pod-bound service account tokens carry the pod in the TokenReview's extra info.
const (
	extraPodName = "authentication.kubernetes.io/pod-name"
	extraPodUID  = "authentication.kubernetes.io/pod-uid"
)

var (
	errNotAuthenticated = errors.New("token not authenticated")
	errWrongAudience    = errors.New("token not issued for " + Audience)
	errNotPodBound      = errors.New("token not bound to a pod")
	errNotPod           = errors.New("pod not found, replaced or finished")
)

// Caller is the pod a proxy runs in, as its token proves.
type Caller struct {
	Namespace      string
	ServiceAccount string
	Pod            string
}

// Identity is the caller's workload ID.
func (c Caller) Identity() string {
	return bindings.ServiceAccountID(c.Namespace, c.ServiceAccount)
}

// Authenticator turns a bearer token into the calling pod.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (Caller, error)
}

// TokenReviewer authenticates projected service account tokens with a
// TokenReview, then checks the pod they are bound to.
type TokenReviewer struct {
	// Client creates TokenReviews.
	Client client.Client
	// Pods reads pods; an uncached reader, so the controller doesn't cache every pod.
	Pods client.Reader
}

func (t *TokenReviewer) Authenticate(ctx context.Context, token string) (Caller, error) {
	review := &authnv1.TokenReview{Spec: authnv1.TokenReviewSpec{Token: token, Audiences: []string{Audience}}}
	if err := t.Client.Create(ctx, review); err != nil {
		return Caller{}, fmt.Errorf("TokenReview: %w", err)
	}

	st := review.Status
	if !st.Authenticated {
		return Caller{}, fmt.Errorf("%w: %s", errNotAuthenticated, st.Error)
	}

	if !slices.Contains(st.Audiences, Audience) {
		return Caller{}, errWrongAudience
	}

	rest, ok := strings.CutPrefix(st.User.Username, "system:serviceaccount:")
	ns, sa, found := strings.Cut(rest, ":")

	if !ok || !found || ns == "" || sa == "" {
		return Caller{}, fmt.Errorf("%w: user %q is not a service account", errNotAuthenticated, st.User.Username)
	}

	podName, podUID := single(st.User.Extra[extraPodName]), single(st.User.Extra[extraPodUID])
	if podName == "" || podUID == "" {
		return Caller{}, errNotPodBound
	}

	var pod corev1.Pod
	if err := t.Pods.Get(ctx, client.ObjectKey{Namespace: ns, Name: podName}, &pod); err != nil {
		return Caller{}, fmt.Errorf("%w: %w", errNotPod, err)
	}

	if string(pod.UID) != podUID || pod.Spec.ServiceAccountName != sa ||
		pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return Caller{}, errNotPod
	}

	return Caller{Namespace: ns, ServiceAccount: sa, Pod: podName}, nil
}

func single(v authnv1.ExtraValue) string {
	if len(v) == 1 {
		return v[0]
	}

	return ""
}
