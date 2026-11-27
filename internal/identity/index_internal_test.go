// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"go.riptides.io/waterwheel/api/v1alpha1"
)

const testSystem = "waterwheel-system"

var testEpoch = time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)

// testWID is a WorkloadIdentity created age minutes after testEpoch.
func testWID(t *testing.T, ns, name, id string, age int, entries []map[string]string) v1alpha1.WorkloadIdentity {
	t.Helper()

	spec, err := json.Marshal(map[string]any{"workloadID": id, "selectors": entries})
	if err != nil {
		t.Fatal(err)
	}

	w := v1alpha1.WorkloadIdentity{ObjectMeta: metav1.ObjectMeta{
		Namespace: ns, Name: name,
		CreationTimestamp: metav1.NewTime(testEpoch.Add(time.Duration(age) * time.Minute)),
	}}
	if err := json.Unmarshal(spec, &w.Spec); err != nil {
		t.Fatal(err)
	}

	return w
}

// naiveBuild compares every entry with every other, as Build once did.
func naiveBuild(wids []v1alpha1.WorkloadIdentity, systemNamespace string) *Set {
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

next:
	for i := range sorted {
		id, problem := s.parse(&sorted[i])
		if problem != nil {
			s.Problems[types.NamespacedName{Namespace: sorted[i].Namespace, Name: sorted[i].Name}] = *problem

			continue
		}

		for _, other := range s.Identities {
			if other.WorkloadID == id.WorkloadID {
				continue
			}

			for n, e := range id.Entries {
				if slices.ContainsFunc(other.Entries, func(o Entry) bool { return maps.Equal(e, o) }) {
					s.Problems[id.Key()] = Problem{ReasonSelectorsOverlap, fmt.Sprintf(
						"selector entry %d is the same as one of %s's, which gives %s; the older WorkloadIdentity wins",
						n+1, other.Key(), other.WorkloadID)}

					continue next
				}
			}
		}

		s.Identities = append(s.Identities, id)
	}

	for _, general := range s.Identities {
		for _, specific := range s.Identities {
			if general.WorkloadID != specific.WorkloadID && slices.ContainsFunc(general.Entries, func(g Entry) bool {
				return slices.ContainsFunc(specific.Entries, g.strictSubsetOf)
			}) {
				s.Shadowed[general.Key()] = append(s.Shadowed[general.Key()], specific.Key())
			}
		}
	}

	return s
}

// The index must decide exactly what comparing every pair of entries decides.
func TestBuildAgreesWithComparingEveryEntry(t *testing.T) {
	t.Parallel()

	rnd := rand.New(rand.NewPCG(1, 2))
	keys := []string{KeyServiceAccount, KeyPodName, "k8s:label:x", "k8s:label:y", "k8s:label:z"}

	var overlaps, shadowed int

	for round := range 200 {
		count := 5 + rnd.IntN(40)
		wids := make([]v1alpha1.WorkloadIdentity, 0, count)

		for i := range count {
			ns := []string{testSystem, "a", "b"}[rnd.IntN(3)]
			id := fmt.Sprintf("ns/%s/w%d", ns, rnd.IntN(4))

			entries := make([]map[string]string, 1+rnd.IntN(4))
			for n := range entries {
				entries[n] = map[string]string{}
				for range rnd.IntN(4) {
					entries[n][keys[rnd.IntN(len(keys))]] = []string{"1", "2"}[rnd.IntN(2)]
				}

				if len(entries[n]) == 0 || rnd.IntN(4) == 0 {
					entries[n][KeyNamespace] = []string{"a", "b"}[rnd.IntN(2)]
				}
			}

			wids = append(wids, testWID(t, ns, fmt.Sprintf("i%d", i), id, rnd.IntN(10), entries))
		}

		want, got := naiveBuild(wids, testSystem), Build(wids, testSystem)

		if !reflect.DeepEqual(got.Identities, want.Identities) {
			t.Fatalf("round %d: identities in effect differ", round)
		}

		if !reflect.DeepEqual(got.Problems, want.Problems) {
			t.Fatalf("round %d: problems\n got %v\nwant %v", round, got.Problems, want.Problems)
		}

		if !reflect.DeepEqual(got.Shadowed, want.Shadowed) {
			t.Fatalf("round %d: shadowed\n got %v\nwant %v", round, got.Shadowed, want.Shadowed)
		}

		shadowed += len(want.Shadowed)

		for _, p := range want.Problems {
			if p.Reason == ReasonSelectorsOverlap {
				overlaps++
			}
		}
	}

	if overlaps < 100 || shadowed < 100 {
		t.Errorf("the rounds had %d overlaps and %d shadowed identities: too few to compare", overlaps, shadowed)
	}
}

// A namespace full of WorkloadIdentities must not make Build, which every
// bindings request needs, compare each entry with every other.
func TestBuildCostOfManyIdentities(t *testing.T) {
	t.Parallel()

	const (
		identities = 200
		entries    = 32
		labels     = 15
	)

	shapes := map[string]func(i, e, k int, rnd *rand.Rand) string{
		// Entries differ in one label.
		"one label differs": func(i, e, k int, _ *rand.Rand) string {
			if k == 0 {
				return fmt.Sprintf("i%de%d", i, e)
			}

			return "v"
		},
		// No key and value is rare, and entries have different sizes.
		"no rare label": func(_, _, _ int, rnd *rand.Rand) string {
			return []string{"", "1", "2", "3"}[rnd.IntN(4)]
		},
	}

	for name, value := range shapes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			rnd := rand.New(rand.NewPCG(3, 4))
			wids := make([]v1alpha1.WorkloadIdentity, 0, identities)

			for i := range identities {
				sel := make([]map[string]string, entries)
				for e := range sel {
					sel[e] = map[string]string{KeyServiceAccount: "sa"}

					for k := range labels {
						if v := value(i, e, k, rnd); v != "" {
							sel[e][fmt.Sprintf("k8s:label:k%02d", k)] = v
						}
					}
				}

				wids = append(wids, testWID(t, "evil", fmt.Sprintf("w%d", i), fmt.Sprintf("ns/evil/w%d", i), i, sel))
			}

			set := Build(wids, testSystem)

			if len(set.Identities) < identities*9/10 {
				t.Errorf("only %d of %d identities are in effect", len(set.Identities), identities)
			}

			// Comparing every pair is about 20 million comparisons.
			if total := identities * entries; set.compared > 4*total {
				t.Errorf("Build compared %d pairs of entries, for %d entries", set.compared, total)
			}

			t.Logf("%d identities in effect, %d pairs of entries compared", len(set.Identities), set.compared)
		})
	}
}

// A Builder builds once for the same objects, and again when one changes.
func TestBuilderKeepsTheSet(t *testing.T) {
	t.Parallel()

	const one, two = "one", "two"

	wids := []v1alpha1.WorkloadIdentity{
		testWID(t, "a", one, "ns/a/one", 1, []map[string]string{{KeyServiceAccount: one}}),
		testWID(t, "a", two, "ns/a/two", 2, []map[string]string{{KeyServiceAccount: two}}),
	}

	b := &Builder{SystemNamespace: testSystem}

	if first, second := b.Build(wids), b.Build(wids); first == second {
		t.Error("kept a Set of objects without UIDs")
	}

	for i := range wids {
		wids[i].UID, wids[i].Generation = types.UID(wids[i].Name), 1
	}

	first := b.Build(wids)

	reversed := slices.Clone(wids)
	slices.Reverse(reversed)

	if b.Build(reversed) != first {
		t.Error("built again for the same objects")
	}

	wids[1] = testWID(t, "a", two, "ns/a/one", 2, []map[string]string{{KeyServiceAccount: two}})
	wids[1].UID, wids[1].Generation = two, 2

	changed := b.Build(wids)
	if changed == first || !changed.Has("a", "ns/a/one") || changed.Has("a", "ns/a/two") {
		t.Error("kept the Set after a spec changed")
	}
}
