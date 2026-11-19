// Package rng provides the deterministic random source used by every
// simulated stage. splitmix64 has no global state, so identical seeds produce
// identical sequences on every platform and every Go release.
package rng

// Source is a splitmix64 generator.
type Source struct {
	state uint64
}

// New seeds a generator. Two sources with the same seed are identical.
func New(seed uint64) *Source {
	return &Source{state: seed ^ 0x9E3779B97F4A7C15}
}

// Uint64 returns the next value.
func (s *Source) Uint64() uint64 {
	s.state += 0x9E3779B97F4A7C15
	z := s.state
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

// Float returns a value in [0,1).
func (s *Source) Float() float64 {
	return float64(s.Uint64()>>11) / float64(uint64(1)<<53)
}

// Symmetric returns a value in [-1,1).
func (s *Source) Symmetric() float64 { return s.Float()*2 - 1 }

// Intn returns a value in [0,n). n<=1 always yields 0.
func (s *Source) Intn(n int) int {
	if n <= 1 {
		return 0
	}
	return int(s.Uint64() % uint64(n))
}

// Chance reports true with probability p, clamped to [0,1].
func (s *Source) Chance(p float64) bool {
	if p <= 0 {
		return false
	}
	if p >= 1 {
		return true
	}
	return s.Float() < p
}
