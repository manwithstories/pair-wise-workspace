// Package randx provides a small, deterministic, dependency-free PRNG.
// Every fixture and every injected anomaly is driven from a seed so that
// repeated runs with the same seed produce byte-identical frames, packet
// loss patterns and error orderings.
package randx

// PCG64 is a permuted congruential generator (PCG-XSH-RR, 64-bit stream).
// It is intentionally tiny and fixed: its sequence must never change, since
// frame hashes from older fixtures have to stay reproducible.
type PCG64 struct {
	state uint64
	inc   uint64
}

const pcgMultiplier = 6364136223846793005

// NewPCG64 seeds a stream. The odd increment keeps the LCG in its maximal
// period regardless of the chosen seed.
func NewPCG64(seed uint64) *PCG64 {
	p := &PCG64{inc: 1442695040888963407 | 1}
	p.state = 0
	_ = p.Uint64()
	p.state += seed
	_ = p.Uint64()
	return p
}

// Uint64 returns the next pseudo-random 64-bit value.
func (p *PCG64) Uint64() uint64 {
	old := p.state
	p.state = old*pcgMultiplier + p.inc
	xorshifted := ((old >> 18) ^ old) >> 27
	rot := old >> 59
	return (xorshifted >> rot) | (xorshifted << ((-rot) & 31))
}

// Intn returns a value in [0,n); n must be positive.
func (p *PCG64) Intn(n int) int {
	if n <= 0 {
		panic("randx: Intn bound must be positive")
	}
	return int(p.Uint64() % uint64(n))
}

// Float64 returns a value in [0.0,1.0).
func (p *PCG64) Float64() float64 {
	return float64(p.Uint64()>>11) / float64(uint64(1)<<53)
}

// Perm returns a deterministic permutation of [0,n).
func (p *PCG64) Perm(n int) []int {
	out := make([]int, n)
	for i := 1; i < n; i++ {
		j := p.Intn(i + 1)
		out[i] = out[j]
		out[j] = i
	}
	return out
}
