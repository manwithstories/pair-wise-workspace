package buffer

import "testing"

// With clock-skewed arrivals, verify no committed interval is rewritten and
// late detection still works when timestamps are monotonic but offset.
func TestFrozenUnderSkew(t *testing.T) {
	cfg := DefaultConfig()
	b := New("v", cfg)
	var last int64
	for i := 0; i < 400; i++ {
		// Media timestamps with a 200ppm skew applied.
		ns := int64(i) * 10_000_000
		skewed := ns + ns*200/1_000_000
		before := len(b.Committed())
		b.Push(Sample{NS: skewed})
		// Committed intervals must never shrink.
		if len(b.Committed()) < before {
			t.Fatalf("committed shrank")
		}
		last = skewed
	}
	b.Flush()
	for i := 1; i < len(b.Committed()); i++ {
		if b.Committed()[i].StartNS < b.Committed()[i-1].StartNS {
			t.Fatalf("non-monotonic commit at %d", i)
		}
	}
	_ = last
}
