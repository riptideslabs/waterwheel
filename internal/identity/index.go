// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"hash/maphash"
	"maps"
)

// index finds equal and more specific entries among the identities in effect
// without comparing every entry with every other: a namespace may hold
// thousands of them.
type index struct {
	seed    maphash.Seed
	entries []*indexedEntry  // distinct entries
	bySum   map[uint64][]int // entries by their hash
	byPair  map[uint64][]int // entries by each key and value they have
	// compared counts the pairs of entries compared key by key.
	compared int
}

type indexedEntry struct {
	entryHash

	entry Entry
	// owners are the identities in effect with the entry, oldest first, as
	// positions in Set.Identities. They all give the same workload ID.
	owners []int
}

type entryHash struct {
	pairs []uint64 // the hash of each key and value
	sum   uint64   // equal entries have the same sum
	bits  uint64   // a bit per pair: the bits of a subset are a subset
}

func newIndex() *index {
	return &index{seed: maphash.MakeSeed(), bySum: map[uint64][]int{}, byPair: map[uint64][]int{}}
}

func (x *index) hashes(entries []Entry) []entryHash {
	out := make([]entryHash, len(entries))

	var h maphash.Hash

	h.SetSeed(x.seed)

	for n, e := range entries {
		eh := entryHash{pairs: make([]uint64, 0, len(e))}

		for k, v := range e {
			h.Reset()
			_, _ = h.WriteString(k)
			_ = h.WriteByte(0)
			_, _ = h.WriteString(v)

			p := h.Sum64()
			eh.pairs = append(eh.pairs, p)
			eh.sum += p
			eh.bits |= 1 << (p % 64)
		}

		out[n] = eh
	}

	return out
}

// find returns the position of an entry equal to e, or -1.
func (x *index) find(e Entry, sum uint64) int {
	for _, i := range x.bySum[sum] {
		x.compared++

		if maps.Equal(x.entries[i].entry, e) {
			return i
		}
	}

	return -1
}

// add records the entries of the identity at position owner.
func (x *index) add(entries []Entry, hashes []entryHash, owner int) {
	for n, e := range entries {
		if i := x.find(e, hashes[n].sum); i >= 0 {
			ie := x.entries[i]
			if ie.owners[len(ie.owners)-1] != owner {
				ie.owners = append(ie.owners, owner)
			}

			continue
		}

		i := len(x.entries)
		x.entries = append(x.entries, &indexedEntry{entryHash: hashes[n], entry: e, owners: []int{owner}})
		x.bySum[hashes[n].sum] = append(x.bySum[hashes[n].sum], i)

		for _, p := range hashes[n].pairs {
			x.byPair[p] = append(x.byPair[p], i)
		}
	}
}

// candidates are the entries that may have every key and value of g: the ones
// with the key and value of g that the fewest entries have.
func (x *index) candidates(g *indexedEntry) []int {
	var fewest []int

	for n, p := range g.pairs {
		if list := x.byPair[p]; n == 0 || len(list) < len(fewest) {
			fewest = list
		}
	}

	if len(g.pairs) > 0 {
		return fewest
	}

	// An entry without keys matches every pod.
	all := make([]int, len(x.entries))
	for i := range all {
		all[i] = i
	}

	return all
}
