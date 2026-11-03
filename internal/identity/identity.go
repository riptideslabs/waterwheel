// SPDX-License-Identifier: Apache-2.0

// Package identity decides which WorkloadIdentity a pod gets with a SPIFFE
// identity source. The controller uses it both to register identities with
// SPIRE and to tell each proxy which SVID to ask for, so the two agree.
//
// A pod gets the workload ID of the WorkloadIdentity whose selector entry
// matches it. When entries of several identities match, the most specific
// entry wins: one that has every key and value of the others, and more. When
// none is most specific, the pod gets no identity.
//
// Two identities with the same selector entry and different workload IDs
// would always tie, so the newer one isn't put into effect. That is the rule
// the platform's webhook applies at admission, applied here as status,
// because Waterwheel validates nothing across objects at admission.
package identity

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

	"go.riptides.io/waterwheel/api/v1alpha1"
)

// Selector keys the SPIRE source supports. All of them describe the pod.
const (
	KeyNamespace      = "k8s:pod:namespace"
	KeyPodName        = "k8s:pod:name"
	KeyServiceAccount = "k8s:pod:serviceaccount"
	KeyLabelPrefix    = "k8s:label:"
)

// Reasons a WorkloadIdentity isn't in effect.
const (
	ReasonReservedWorkloadID  = "ReservedWorkloadID"
	ReasonForeignWorkloadID   = "ForeignWorkloadID"
	ReasonForeignNamespace    = "ForeignNamespace"
	ReasonUnsupportedSelector = "UnsupportedSelector"
	ReasonInvalidSelector     = "InvalidSelector"
	ReasonSelectorsOverlap    = "SelectorsOverlap"
)

var (
	// ErrNoIdentity: no WorkloadIdentity in effect matches the pod.
	ErrNoIdentity = errors.New("no WorkloadIdentity matches the pod")
	// ErrAmbiguous: several match, and none is more specific than the others.
	ErrAmbiguous = errors.New("WorkloadIdentities with different workload IDs match the pod, none more specific than the others")
)

// Pod is what selectors match.
type Pod struct {
	Namespace      string
	Name           string
	ServiceAccount string
	Labels         map[string]string
}

// Entry is one selector entry, with its values as strings. Entries of
// identities outside the system namespace always carry their namespace.
type Entry map[string]string

// Matches reports whether every key of the entry matches the pod.
func (e Entry) Matches(p Pod) bool {
	for k, v := range e {
		var ok bool

		switch k {
		case KeyNamespace:
			ok = p.Namespace == v
		case KeyPodName:
			ok = p.Name == v
		case KeyServiceAccount:
			ok = p.ServiceAccount == v
		default:
			label, found := p.Labels[strings.TrimPrefix(k, KeyLabelPrefix)]
			ok = found && label == v
		}

		if !ok {
			return false
		}
	}

	return true
}

// subsetOf reports whether every key and value of e is in o, so any pod o
// matches, e matches too.
func (e Entry) subsetOf(o Entry) bool {
	for k, v := range e {
		if ov, ok := o[k]; !ok || ov != v {
			return false
		}
	}

	return true
}

func (e Entry) strictSubsetOf(o Entry) bool {
	return len(e) < len(o) && e.subsetOf(o)
}

// Identity is a WorkloadIdentity in effect.
type Identity struct {
	Namespace  string
	Name       string
	WorkloadID string
	Entries    []Entry
	SVID       *v1alpha1.SVIDProperties
}

// Key is the identity's namespace and name.
func (i *Identity) Key() types.NamespacedName {
	return types.NamespacedName{Namespace: i.Namespace, Name: i.Name}
}

// Problem says why a WorkloadIdentity isn't in effect.
type Problem struct {
	Reason  string
	Message string
}

// Set is the WorkloadIdentities in effect, built from all of them.
type Set struct {
	// Identities are in effect, oldest first.
	Identities []*Identity
	// Problems say why the others aren't.
	Problems map[types.NamespacedName]Problem
	// Shadowed lists, for identities in effect, the identities with more
	// specific entries: pods those match get their workload ID instead.
	Shadowed map[types.NamespacedName][]types.NamespacedName

	systemNamespace string
}

// Build works out which WorkloadIdentities are in effect. It takes all of
// them, in every namespace, so the result doesn't depend on which pod asks.
func Build(wids []v1alpha1.WorkloadIdentity, systemNamespace string) *Set {
	s := &Set{
		Problems:        map[types.NamespacedName]Problem{},
		Shadowed:        map[types.NamespacedName][]types.NamespacedName{},
		systemNamespace: systemNamespace,
	}

	sorted := slices.Clone(wids)
	slices.SortFunc(sorted, func(a, b v1alpha1.WorkloadIdentity) int {
		return cmp.Or(a.CreationTimestamp.Compare(b.CreationTimestamp.Time),
			cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})

	for i := range sorted {
		w := &sorted[i]
		key := types.NamespacedName{Namespace: w.Namespace, Name: w.Name}

		id, problem := s.parse(w)
		if problem == nil {
			problem = s.overlap(id)
		}

		if problem != nil {
			s.Problems[key] = *problem

			continue
		}

		s.Identities = append(s.Identities, id)
	}

	for _, general := range s.Identities {
		for _, specific := range s.Identities {
			if general.WorkloadID != specific.WorkloadID && shadows(specific, general) {
				s.Shadowed[general.Key()] = append(s.Shadowed[general.Key()], specific.Key())
			}
		}
	}

	return s
}

// shadows reports whether an entry of specific is more specific than an entry of general.
func shadows(specific, general *Identity) bool {
	return slices.ContainsFunc(general.Entries, func(g Entry) bool {
		return slices.ContainsFunc(specific.Entries, g.strictSubsetOf)
	})
}

// parse checks a WorkloadIdentity on its own: the namespace rules, which
// admission enforces too, and the selectors the SPIRE source supports.
func (s *Set) parse(w *v1alpha1.WorkloadIdentity) (*Identity, *Problem) {
	id := w.Spec.WorkloadID
	system := w.Namespace == s.systemNamespace

	switch {
	case strings.HasPrefix(id, "waterwheel/"):
		return nil, &Problem{ReasonReservedWorkloadID, "the waterwheel/ prefix is reserved for Waterwheel's own identities"}
	case !system && !strings.HasPrefix(id, "ns/"+w.Namespace+"/"):
		return nil, &Problem{ReasonForeignWorkloadID, "workloadID must start with ns/" + w.Namespace + "/"}
	}

	entries := make([]Entry, 0, len(w.Spec.Selectors))

	for n, sel := range w.Spec.Selectors {
		e := Entry{}

		for k, v := range sel {
			if p := checkKey(k, v.String()); p != nil {
				p.Message = fmt.Sprintf("selector entry %d: %s", n+1, p.Message)

				return nil, p
			}

			e[k] = v.String()
		}

		if !system {
			if ns, ok := e[KeyNamespace]; ok && ns != w.Namespace {
				return nil, &Problem{ReasonForeignNamespace, "selectors may only match pods in namespace " + w.Namespace}
			}

			e[KeyNamespace] = w.Namespace
		}

		entries = append(entries, e)
	}

	return &Identity{Namespace: w.Namespace, Name: w.Name, WorkloadID: id, Entries: entries, SVID: w.Spec.SVID}, nil
}

func checkKey(key, value string) *Problem {
	switch {
	case key == KeyNamespace || key == KeyPodName || key == KeyServiceAccount:
		return nil
	case strings.HasPrefix(key, KeyLabelPrefix):
		label := strings.TrimPrefix(key, KeyLabelPrefix)
		errs := append(validation.IsQualifiedName(label), validation.IsValidLabelValue(value)...)

		if len(errs) > 0 {
			return &Problem{ReasonInvalidSelector, fmt.Sprintf("%s=%q: %s", key, value, strings.Join(errs, "; "))}
		}

		return nil
	case strings.HasPrefix(key, "k8s:container:") || strings.HasPrefix(key, "process:"):
		return &Problem{ReasonUnsupportedSelector, key + " would select waterwheel-proxy, which fetches the SVIDs; " +
			"with SPIRE, select pods (k8s:pod:* and k8s:label:*)"}
	}

	return &Problem{ReasonInvalidSelector, "unknown selector key " + key}
}

// overlap reports an entry that is the same as one of an identity already in
// effect, with another workload ID. The older identity wins.
func (s *Set) overlap(id *Identity) *Problem {
	for _, other := range s.Identities {
		if other.WorkloadID == id.WorkloadID {
			continue
		}

		for n, e := range id.Entries {
			if slices.ContainsFunc(other.Entries, func(o Entry) bool { return maps.Equal(e, o) }) {
				return &Problem{ReasonSelectorsOverlap, fmt.Sprintf(
					"selector entry %d is the same as one of %s's, which gives %s; the older WorkloadIdentity wins",
					n+1, other.Key(), other.WorkloadID)}
			}
		}
	}

	return nil
}

// Has reports whether an identity in effect gives workloadID to pods in
// namespace, or with the system namespace, to pods anywhere.
func (s *Set) Has(namespace, workloadID string) bool {
	return slices.ContainsFunc(s.Identities, func(i *Identity) bool {
		return i.WorkloadID == workloadID &&
			(namespace == s.systemNamespace || i.Namespace == namespace || i.Namespace == s.systemNamespace)
	})
}

// Resolve returns the pod's workload ID. Only identities in the pod's
// namespace and in the system namespace apply.
func (s *Set) Resolve(p Pod) (string, error) {
	type match struct {
		id    string
		entry Entry
	}

	var (
		matches []match
		ids     []string
	)

	for _, i := range s.Identities {
		if i.Namespace != p.Namespace && i.Namespace != s.systemNamespace {
			continue
		}

		for _, e := range i.Entries {
			if e.Matches(p) {
				matches = append(matches, match{i.WorkloadID, e})

				if !slices.Contains(ids, i.WorkloadID) {
					ids = append(ids, i.WorkloadID)
				}
			}
		}
	}

	switch len(ids) {
	case 0:
		return "", ErrNoIdentity
	case 1:
		return ids[0], nil
	}

	for _, m := range matches {
		if !slices.ContainsFunc(matches, func(o match) bool { return o.id != m.id && !o.entry.strictSubsetOf(m.entry) }) {
			return m.id, nil
		}
	}

	slices.Sort(ids)

	return "", fmt.Errorf("%w: %s", ErrAmbiguous, strings.Join(ids, ", "))
}

// SPIFFEID is spiffe://<trust domain>/<workload ID>.
func SPIFFEID(trustDomain, workloadID string) string {
	return "spiffe://" + trustDomain + "/" + workloadID
}
