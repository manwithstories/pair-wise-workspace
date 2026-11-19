package clock

import "testing"

// BenchmarkEstimateLargePairSet guards the cost of the robust fit for a
// full clip: the Theil-Sen candidate set is decimated above 200k pairs.
func BenchmarkEstimateLargePairSet(b *testing.B) {
	pairs := make([]Pair, 1500)
	for i := range pairs {
		src := int64(i) * 90000
		pairs[i] = Pair{SourceTicks: src, NodeTicks: src + src*85/1_000_000}
	}
	base := NewBase(1, 90000)
	threshold := NewRational(1, 100)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Estimate(pairs, base, threshold)
	}
}
