// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"fmt"
	"testing"
)

// With a wildcard destination the app picks the names, so the cache must not
// keep a leaf for each spelling of a host, nor grow without end.
func TestLeafCacheIsBounded(t *testing.T) {
	t.Parallel()

	ca, err := LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	first, err := ca.Leaf("api.example.com")
	if err != nil {
		t.Fatal(err)
	}

	for _, spelling := range []string{"API.Example.COM", "api.example.com."} {
		again, err := ca.Leaf(spelling)
		if err != nil {
			t.Fatal(err)
		}

		if again != first {
			t.Errorf("%s got a leaf of its own", spelling)
		}
	}

	var last string

	for i := range maxLeaves + 50 {
		last = fmt.Sprintf("h%d.example.com", i)
		if _, err := ca.Leaf(last); err != nil {
			t.Fatal(err)
		}
	}

	if n := len(ca.leaves); n > maxLeaves {
		t.Errorf("%d leaves cached; want at most %d", n, maxLeaves)
	}

	if _, ok := ca.leaves[last]; !ok {
		t.Error("the newest leaf isn't cached")
	}
}
