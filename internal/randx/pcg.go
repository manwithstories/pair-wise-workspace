// Package randx provides a small, deterministic, dependency-free PRNG.
// Every fixture and every injected anomaly is driven from a seed so that
// repeated runs with the same seed produce byte-identical frames, packet
// loss patterns and error orderings.
package randx

// PCG64 is the 64-bit-state, 32-bit-output PCG-XSH-RR generator
// (pcg32 in the reference C++ library), widened to 64-bit outputs by
// combining two successive 32-bit draws. It uses only uint64 arithmetic,
// so the sequence is identical on every architecture. The constants and
// transitions are fixed: changing them invalidates stored fixtures.
type PCG64 struct {
	state uint64
	inc   uint64
}

const (
	pcgMultiplier = 6364136223846793005
	pcgIncrement  = 1442695040888963407
)

// NewPCG64 seeds a stream with the standard pcg32 seeding routine.
func NewPCG64(seed uint64) *PCG64 {
	p := &PCG64{inc: (pcgIncrement << 1) | 1}
	p.state = 0
	p.uint32()
	p.state += seed
	p.uint32()
	return p
}

// uint32 performs one LCG step and applies the XSH-RR output function.
func (p *PCG64) uint32() uint32 {
	old := p.state
	p.state = old*pcgMultiplier + p.inc
	xorshifted := uint32(((old >> 18) ^ old) >> 27)
	rot := uint32(old >> 59)
	return (xorshifted >> rot) | (xorshifted << ((-rot) & 31))
}

// Uint64 returns the next pseudo-random 64-bit value from two 32-bit draws.
func (p *PCG64) Uint64() uint64 {
	hi := uint64(p.uint32())
	lo := uint64(p.uint32())
	return (hi << 32) | lo
}

// Intn returns a value in [0,n); n must be positive. Lemire-style rejection
// on the 64-bit draw keeps the result free of modulo bias.
func (p *PCG64) Intn(n int) int {
	if n <= 0 {
		panic("randx: Intn bound must be positive")
	}
	nu := uint64(n)
	threshold := (-nu) % nu
	for {
		r := p.Uint64()
		if r%nu >= threshold || threshold == 0 {
			return int(r % nu)
		}
	}
}

// Float64 returns a value in [0.0,1.0) using the top 53 random bits.
func (p *PCG64) Float64() float64 {
	return float64(p.Uint64()>>11) * (1.0 / float64(uint64(1)<<53))
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
