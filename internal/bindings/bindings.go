// SPDX-License-Identifier: Apache-2.0

// Package bindings works out which credentials a workload gets: its
// CredentialBindings, their sources, and the audience each source's subject
// token needs. The controller serves the result to proxies, and the injector
// uses it to pick the pod's projected token audiences.
package bindings

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"go.riptides.io/waterwheel/api/v1alpha1"
)

// Snapshot is everything a proxy needs: the bindings of one workload identity.
type Snapshot struct {
	Identity string `json:"identity"`
	// SPIFFEID is the SVID the proxy asks for, with SPIFFE sources.
	SPIFFEID string `json:"spiffeID,omitempty"`
	// Reason says why the pod has no identity, with SPIFFE sources.
	Reason   string    `json:"reason,omitempty"`
	Bindings []Binding `json:"bindings"`
}

// Binding is one CredentialBinding resolved against its source.
type Binding struct {
	Namespace    string                        `json:"namespace"`
	Name         string                        `json:"name"`
	Source       string                        `json:"source"`
	Provider     v1alpha1.CredentialSourceSpec `json:"provider"`
	Audience     string                        `json:"audience"`
	Destinations []v1alpha1.Destination        `json:"destinations"`
	Header       string                        `json:"header"`
	// Scheme precedes the token in the header; empty means the bare token.
	Scheme string `json:"scheme"`
}

// Reasons a proxy reports.
const (
	ReasonCredentialIssued    = "CredentialIssued"
	ReasonTokenExchangeFailed = "TokenExchangeFailed"
	ReasonNoSubjectToken      = "NoSubjectToken"
	ReasonNoWorkloadIdentity  = "NoWorkloadIdentity"
)

// Report is what a proxy tells the controller when its credentials change,
// for an event on its pod.
type Report struct {
	// Binding is the binding's namespace/name; empty for NoWorkloadIdentity.
	Binding string `json:"binding,omitempty"`
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"`
}

var (
	errNoProvider      = errors.New("the source names no provider")
	errForeignIdentity = errors.New("workloadID is outside the binding's namespace")
)

// ServiceAccountID is the workload ID of a Kubernetes service account.
func ServiceAccountID(namespace, serviceAccount string) string {
	return "ns/" + namespace + "/sa/" + serviceAccount
}

// ParseServiceAccountID is the inverse of ServiceAccountID.
func ParseServiceAccountID(id string) (string, string, bool) {
	rest, ok := strings.CutPrefix(id, "ns/")
	ns, sa, found := strings.Cut(rest, "/sa/")

	if !ok || !found || ns == "" || sa == "" || strings.Contains(ns, "/") || strings.Contains(sa, "/") {
		return "", "", false
	}

	return ns, sa, true
}

// TokenFile names the sidecar's projected token for an audience. Audiences
// are often URLs, so the name is a hash the proxy can compute too.
func TokenFile(audience string) string {
	sum := sha256.Sum256([]byte(audience))

	return "aud-" + hex.EncodeToString(sum[:8])
}

// Audience is the audience of the subject token a source exchanges: the first
// one the source lists, or the provider's default.
func Audience(s v1alpha1.CredentialSourceSpec) (string, error) {
	switch {
	case s.Anthropic != nil:
		return first(s.Anthropic.IDTokenAudience, "https://api.anthropic.com"), nil
	case s.OpenAI != nil:
		return first(s.OpenAI.IDTokenAudience, "https://api.openai.com"), nil
	case s.GCP != nil:
		// The workload identity pool provider's default allowed audience.
		return first(s.GCP.Audience, "https:"+s.GCP.OIDCProviderID), nil
	case s.Azure != nil:
		return first(s.Azure.Audience, "api://AzureADTokenExchange"), nil
	case s.RFC7523 != nil:
		return first(s.RFC7523.IDTokenAudience, s.RFC7523.TokenEndpointURL), nil
	case s.RFC8693 != nil:
		return first(s.RFC8693.IDTokenAudience, s.RFC8693.TokenEndpointURL), nil
	}

	return "", errNoProvider
}

func first(list []string, def string) string {
	if len(list) > 0 {
		return list[0]
	}

	return def
}

// Check reports why a binding can't be used, or nil. Admission already
// enforces the namespace rule; this checks it again, so deleting the policy
// doesn't open the boundary.
func Check(b *v1alpha1.CredentialBinding, systemNamespace string) error {
	if b.Namespace != systemNamespace && !strings.HasPrefix(b.Spec.WorkloadID, "ns/"+b.Namespace+"/") {
		return errForeignIdentity
	}

	return nil
}

// Resolve turns a binding into what the proxy needs, or reports why it can't.
// A missing source is a NotFound error.
func Resolve(ctx context.Context, c client.Reader, b *v1alpha1.CredentialBinding) (Binding, error) {
	var src v1alpha1.CredentialSource
	if err := c.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: b.Spec.CredentialSource}, &src); err != nil {
		return Binding{}, err
	}

	aud, err := Audience(src.Spec)
	if err != nil {
		return Binding{}, fmt.Errorf("CredentialSource %s: %w", src.Name, err)
	}

	inj := b.Spec.Propagation.Injection

	dests := make([]v1alpha1.Destination, 0, len(inj.Destinations))
	for _, d := range inj.Destinations {
		if d.Port == 0 {
			d.Port = 443
		}

		dests = append(dests, d)
	}

	scheme := cmp.Or(inj.Scheme, "Bearer")
	if scheme == "-" {
		scheme = ""
	}

	return Binding{
		Namespace:    b.Namespace,
		Name:         b.Name,
		Source:       src.Name,
		Provider:     src.Spec,
		Audience:     aud,
		Destinations: dests,
		Header:       cmp.Or(inj.Header, "Authorization"),
		Scheme:       scheme,
	}, nil
}

// For returns the usable bindings of a workload identity in a namespace: the
// matching bindings there, and those in the system namespace. Bindings that
// fail Check or whose source is missing are left out; their status says why.
func For(ctx context.Context, c client.Reader, systemNamespace, namespace, identity string) (Snapshot, error) {
	snap := Snapshot{Identity: identity, Bindings: []Binding{}}

	namespaces := []string{namespace}
	if systemNamespace != namespace {
		namespaces = append(namespaces, systemNamespace)
	}

	for _, ns := range namespaces {
		var list v1alpha1.CredentialBindingList
		if err := c.List(ctx, &list, client.InNamespace(ns)); err != nil {
			return Snapshot{}, err
		}

		for i := range list.Items {
			b := &list.Items[i]
			if b.Spec.WorkloadID != identity || Check(b, systemNamespace) != nil {
				continue
			}

			r, err := Resolve(ctx, c, b)
			if apierrors.IsNotFound(err) {
				continue
			}

			if err != nil {
				return Snapshot{}, err
			}

			snap.Bindings = append(snap.Bindings, r)
		}
	}

	slices.SortFunc(snap.Bindings, func(a, b Binding) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})

	return snap, nil
}

// Audiences returns the distinct subject token audiences of a snapshot, sorted.
func (s Snapshot) Audiences() []string {
	auds := make([]string, 0, len(s.Bindings))
	for _, b := range s.Bindings {
		auds = append(auds, b.Audience)
	}

	slices.Sort(auds)

	return slices.Compact(auds)
}
