package fixture

import "math/rand"

// planBuilder turns a seed into a deterministic packet-order/loss plan. The
// PRNG is the standard library source so a given seed always reproduces the
// exact same sequence across machines and Go versions in the module.
type planBuilder struct {
	rng *rand.Rand
}

func newPlanBuilder(seed int64) *planBuilder {
	return &planBuilder{rng: rand.New(rand.NewSource(seed))}
}

// shuffle returns a permuted copy of seqs using Fisher-Yates.
func (b *planBuilder) shuffle(seqs []uint16) []uint16 {
	out := append([]uint16(nil), seqs...)
	for i := len(out) - 1; i > 0; i-- {
		j := b.rng.Intn(i + 1)
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// pick returns n distinct sequence numbers drawn from pool.
func (b *planBuilder) pick(pool []uint16, n int) []uint16 {
	if n > len(pool) {
		n = len(pool)
	}
	idx := b.rng.Perm(len(pool))[:n]
	out := make([]uint16, 0, n)
	for _, i := range idx {
		out = append(out, pool[i])
	}
	sortU16(out)
	return out
}

// contiguous picks a run of length n starting inside the pool, used to model
// a burst loss that a single XOR block cannot repair.
func (b *planBuilder) burst(pool []uint16, n int) []uint16 {
	if n >= len(pool) {
		n = len(pool) - 1
	}
	start := b.rng.Intn(len(pool) - n + 1)
	out := append([]uint16(nil), pool[start:start+n]...)
	sortU16(out)
	return out
}

func sortU16(a []uint16) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j-1] > a[j]; j-- {
			a[j-1], a[j] = a[j], a[j-1]
		}
	}
}

func containsU16(haystack []uint16, needle uint16) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

// removeU16 returns a copy of seqs with the given values dropped.
func removeU16(seqs, drop []uint16) []uint16 {
	out := make([]uint16, 0, len(seqs))
	for _, s := range seqs {
		if !containsU16(drop, s) {
			out = append(out, s)
		}
	}
	return out
}
