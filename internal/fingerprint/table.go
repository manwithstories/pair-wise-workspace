package fingerprint

import (
	"fmt"
	"sort"
)

// Table maps normalized content hashes to a shared storage reference.
// Equal content shares one StorageID while each fragment keeps its own
// position on the timeline; the first occurrence wins deterministically.
type Table struct {
	storage map[uint64]string
	hits    map[string]int // storage id -> number of duplicate hits
	order   []uint64       // encounter order for deterministic iteration
}

// NewTable creates an empty dedupe table.
func NewTable() *Table {
	return &Table{storage: map[uint64]string{}, hits: map[string]int{}}
}

// Register records a fragment's hash and returns its shared storage id and
// whether this is a duplicate (true when the content was already seen).
func (t *Table) Register(hash uint64) (storageID string, duplicate bool) {
	if id, ok := t.storage[hash]; ok {
		t.hits[id]++
		return id, true
	}
	id := fmt.Sprintf("store/%016x", hash)
	t.storage[hash] = id
	t.order = append(t.order, hash)
	return id, false
}

// Hits returns the number of duplicate registrations (fragments sharing a
// storage reference beyond its first occurrence).
func (t *Table) Hits() int {
	n := 0
	for _, c := range t.hits {
		n += c
	}
	return n
}

// StorageIDs returns the distinct storage references in encounter order.
func (t *Table) StorageIDs() []string {
	out := make([]string, 0, len(t.order))
	for _, h := range t.order {
		out = append(out, t.storage[h])
	}
	return out
}

// SortedHashes exposes the registered hashes for deterministic reports.
func (t *Table) SortedHashes() []uint64 {
	out := append([]uint64(nil), t.order...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
