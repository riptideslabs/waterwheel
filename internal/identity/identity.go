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
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

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
	ReasonForeignDNSName      = "ForeignDNSName"
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
	clusterDomain   string
	// compared counts the pairs of entries Build compared key by key.
	compared int
}

// Build works out which WorkloadIdentities are in effect. It takes all of
// them, in every namespace, so the result doesn't depend on which pod asks.
func Build(wids []v1alpha1.WorkloadIdentity, systemNamespace string) *Set {
	return (&Builder{SystemNamespace: systemNamespace}).build(wids)
}

// Builder builds Sets, and keeps the last one while the WorkloadIdentities
// stay the same: every bindings request needs the Set, and few change it.
type Builder struct {
	SystemNamespace string
	// ClusterDomain is the cluster's DNS domain; cluster.local if empty.
	ClusterDomain string

	mu   sync.Mutex
	key  [sha256.Size]byte
	last *Set
}

// Build is the package's Build. Callers may get the same Set, so they must
// not change it.
func (b *Builder) Build(wids []v1alpha1.WorkloadIdentity) *Set {
	key, ok := specsOf(wids)
	if !ok {
		return b.build(wids)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.last == nil || b.key != key {
		b.last, b.key = b.build(wids), key
	}

	return b.last
}

// specsOf identifies the specs of wids by their UIDs and generations. Objects
// that didn't come from the API server have neither.
func specsOf(wids []v1alpha1.WorkloadIdentity) ([sha256.Size]byte, bool) {
	ids := make([]string, 0, len(wids))

	for i := range wids {
		w := &wids[i]
		if w.UID == "" || w.Generation == 0 {
			return [sha256.Size]byte{}, false
		}

		ids = append(ids, string(w.UID)+"/"+strconv.FormatInt(w.Generation, 10))
	}

	slices.Sort(ids)

	return sha256.Sum256([]byte(strings.Join(ids, "\n"))), true
}

func (b *Builder) build(wids []v1alpha1.WorkloadIdentity) *Set {
	s := &Set{
		Problems:        map[types.NamespacedName]Problem{},
		Shadowed:        map[types.NamespacedName][]types.NamespacedName{},
		systemNamespace: b.SystemNamespace,
		clusterDomain:   cmp.Or(b.ClusterDomain, "cluster.local"),
	}

	sorted := slices.Clone(wids)
	slices.SortFunc(sorted, func(a, b v1alpha1.WorkloadIdentity) int {
		return cmp.Or(a.CreationTimestamp.Compare(b.CreationTimestamp.Time),
			cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})

	x := newIndex()

	for i := range sorted {
		w := &sorted[i]
		key := types.NamespacedName{Namespace: w.Namespace, Name: w.Name}

		id, problem := s.parse(w)

		var hashes []entryHash

		if problem == nil {
			hashes = x.hashes(id.Entries)
			problem = s.overlap(x, id, hashes)
		}

		if problem != nil {
			s.Problems[key] = *problem

			continue
		}

		x.add(id.Entries, hashes, len(s.Identities))
		s.Identities = append(s.Identities, id)
	}

	s.shadow(x)
	s.compared = x.compared

	return s
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

	if p := s.checkDNSNames(w); p != nil {
		return nil, p
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

	// A copy: the Set may outlive the objects it was built from.
	return &Identity{Namespace: w.Namespace, Name: w.Name, WorkloadID: id, Entries: entries, SVID: w.Spec.SVID.DeepCopy()}, nil
}

// checkDNSNames keeps an X.509-SVID's DNS names to the Services of the
// identity's namespace. A certificate with another namespace's Service name
// would pass for that Service wherever the trust bundle is trusted.
func (s *Set) checkDNSNames(w *v1alpha1.WorkloadIdentity) *Problem {
	if w.Namespace == s.systemNamespace || w.Spec.SVID == nil || w.Spec.SVID.X509 == nil {
		return nil
	}

	svc := "." + w.Namespace + ".svc"

	for _, d := range w.Spec.SVID.X509.DNSNames {
		name := strings.ToLower(d)
		if !strings.HasSuffix(name, svc) && !strings.HasSuffix(name, svc+"."+s.clusterDomain) {
			return &Problem{ReasonForeignDNSName, fmt.Sprintf(
				"svid.x509.dnsNames must end with %s or %s.%s, unlike %s", svc, svc, s.clusterDomain, d)}
		}
	}

	return nil
}

func checkKey(key, value string) *Problem {
	switch {
	case key == KeyNamespace:
		return nil
	case key == KeyPodName || key == KeyServiceAccount:
		// Only a name can match one, and SPIRE reads these values as templates.
		if errs := validation.IsDNS1123Subdomain(value); len(errs) > 0 {
			return &Problem{ReasonInvalidSelector, fmt.Sprintf("%s=%q: %s", key, value, strings.Join(errs, "; "))}
		}

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
func (s *Set) overlap(x *index, id *Identity, hashes []entryHash) *Problem {
	other, entry := -1, 0

	for n, e := range id.Entries {
		i := x.find(e, hashes[n].sum)
		if i < 0 {
			continue
		}

		// Every identity with the entry gives the same workload ID.
		oldest := x.entries[i].owners[0]
		if s.Identities[oldest].WorkloadID != id.WorkloadID && (other < 0 || oldest < other) {
			other, entry = oldest, n
		}
	}

	if other < 0 {
		return nil
	}

	o := s.Identities[other]

	return &Problem{ReasonSelectorsOverlap, fmt.Sprintf(
		"selector entry %d is the same as one of %s's, which gives %s; the older WorkloadIdentity wins",
		entry+1, o.Key(), o.WorkloadID)}
}

// shadow fills in Shadowed: for each identity, the identities that give
// another workload ID and have an entry more specific than one of its own.
func (s *Set) shadow(x *index) {
	words := (len(s.Identities) + 63) / 64
	specifics := map[int][]uint64{} // by general identity, a bit per specific one

	for _, g := range x.entries {
		for _, c := range x.candidates(g) {
			e := x.entries[c]
			if !s.moreSpecific(x, e, g, specifics) {
				continue
			}

			for _, general := range g.owners {
				if specifics[general] == nil {
					specifics[general] = make([]uint64, words)
				}

				for _, specific := range e.owners {
					specifics[general][specific/64] |= 1 << (specific % 64)
				}
			}
		}
	}

	for general, bits := range specifics {
		var keys []types.NamespacedName

		for i, id := range s.Identities {
			if hasBit(bits, i) {
				keys = append(keys, id.Key())
			}
		}

		s.Shadowed[s.Identities[general].Key()] = keys
	}
}

// moreSpecific reports whether e has every key and value of g, and more, and
// gives another workload ID. It compares them only when the cheap checks pass,
// and known holds what other entries of the same identities already showed.
func (s *Set) moreSpecific(x *index, e, g *indexedEntry, known map[int][]uint64) bool {
	if len(e.entry) <= len(g.entry) || g.bits&^e.bits != 0 ||
		s.Identities[g.owners[0]].WorkloadID == s.Identities[e.owners[0]].WorkloadID {
		return false
	}

	if len(g.owners) == 1 && len(e.owners) == 1 && hasBit(known[g.owners[0]], e.owners[0]) {
		return false
	}

	x.compared++

	return g.entry.subsetOf(e.entry)
}

func hasBit(bits []uint64, i int) bool {
	return bits != nil && bits[i/64]&(1<<(i%64)) != 0
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
