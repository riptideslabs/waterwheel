// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"go.riptides.io/waterwheel/internal/bindings"
	"go.riptides.io/waterwheel/internal/identity"
)

// Audience is the audience of the projected token proxies present to the
// controller.
const Audience = "waterwheel-controller"

// Pod-bound service account tokens carry the pod in the TokenReview's extra info.
const (
	extraPodName = "authentication.kubernetes.io/pod-name"
	extraPodUID  = "authentication.kubernetes.io/pod-uid"
)

// ErrAuthUnavailable is a failure to check a token, such as a Kubernetes API
// error, rather than a token that fails the check. Callers may try again.
var ErrAuthUnavailable = errors.New("can't check the token now")

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
	PodUID         types.UID
	Labels         map[string]string
}

// Identity is the caller's workload ID with service account tokens.
func (c Caller) Identity() string {
	return bindings.ServiceAccountID(c.Namespace, c.ServiceAccount)
}

// Selectable is what WorkloadIdentity selectors match.
func (c Caller) Selectable() identity.Pod {
	return identity.Pod{Namespace: c.Namespace, Name: c.Pod, ServiceAccount: c.ServiceAccount, Labels: c.Labels}
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
	if !plausible(token) {
		return Caller{}, fmt.Errorf("%w: not an unexpired token for %s", errNotAuthenticated, Audience)
	}

	review := &authnv1.TokenReview{Spec: authnv1.TokenReviewSpec{Token: token, Audiences: []string{Audience}}}
	if err := t.Client.Create(ctx, review); err != nil {
		return Caller{}, fmt.Errorf("%w: TokenReview: %w", ErrAuthUnavailable, err)
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
	if err := t.Pods.Get(ctx, client.ObjectKey{Namespace: ns, Name: podName}, &pod); apierrors.IsNotFound(err) {
		return Caller{}, fmt.Errorf("%w: %w", errNotPod, err)
	} else if err != nil {
		return Caller{}, fmt.Errorf("%w: reading the pod: %w", ErrAuthUnavailable, err)
	}

	if string(pod.UID) != podUID || pod.Spec.ServiceAccountName != sa ||
		pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return Caller{}, errNotPod
	}

	return Caller{Namespace: ns, ServiceAccount: sa, Pod: podName, PodUID: pod.UID, Labels: pod.Labels}, nil
}

// plausible reports whether token could pass the review: a JWT for the
// controller that hasn't expired. Anyone can reach the API, and this spares
// the API server a TokenReview for whatever else they send. Nothing here is
// verified; the review decides.
func plausible(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}

	var claims struct {
		Aud any     `json:"aud"`
		Exp float64 `json:"exp"`
	}

	if json.Unmarshal(payload, &claims) != nil {
		return false
	}

	// A minute's leeway, for a clock that differs from the API server's.
	if time.Since(time.Unix(int64(claims.Exp), 0)) > time.Minute {
		return false
	}

	switch aud := claims.Aud.(type) {
	case string:
		return aud == Audience
	case []any:
		return slices.Contains(aud, any(Audience))
	}

	return false
}

func single(v authnv1.ExtraValue) string {
	if len(v) == 1 {
		return v[0]
	}

	return ""
}
