package randx

import "testing"

func TestPCGDeterministic(t *testing.T) {
	a := NewPCG64(41)
	b := NewPCG64(41)
	for i := 0; i < 1000; i++ {
		if a.Uint64() != b.Uint64() {
			t.Fatalf("diverged at %d", i)
		}
	}
	c := NewPCG64(42)
	if NewPCG64(41).Uint64() == c.Uint64() {
		t.Fatal("different seeds produced same first value")
	}
}

func TestIntnAndFloat(t *testing.T) {
	p := NewPCG64(7)
	for i := 0; i < 1000; i++ {
		n := p.Intn(13)
		if n < 0 || n >= 13 {
			t.Fatalf("Intn out of range: %d", n)
		}
		f := p.Float64()
		if f < 0 || f >= 1 {
			t.Fatalf("Float64 out of range: %v", f)
		}
	}
}

func TestPerm(t *testing.T) {
	p := NewPCG64(99)
	got := p.Perm(10)
	seen := make(map[int]bool)
	if len(got) != 10 {
		t.Fatal("wrong perm length")
	}
	for _, v := range got {
		if seen[v] {
			t.Fatalf("duplicate value %d", v)
		}
		seen[v] = true
	}
	other := NewPCG64(99).Perm(10)
	for i := range got {
		if got[i] != other[i] {
			t.Fatal("permutation not deterministic")
		}
	}
}

func TestIntnPanic(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on non-positive bound")
		}
	}()
	NewPCG64(1).Intn(0)
}

func TestFloat64Uniform(t *testing.T) {
	// A regression guard: an earlier hand-rolled output function put ~83% of
	// draws in the lowest decile. The real PCG output must spread evenly.
	p := NewPCG64(77)
	const n = 100000
	buckets := [10]int{}
	low := 0
	for i := 0; i < n; i++ {
		f := p.Float64()
		if f < 0 || f >= 1 {
			t.Fatalf("out of range: %v", f)
		}
		if f < 0.02 {
			low++
		}
		b := int(f * 10)
		if b > 9 {
			b = 9
		}
		buckets[b]++
	}
	// Expect ~2000 below 0.02; allow generous slack for CI stability.
	if low < 1200 || low > 2800 {
		t.Fatalf("low-tail biased: %d below 0.02", low)
	}
	for i, c := range buckets {
		if c < 8000 || c > 12000 {
			t.Fatalf("bucket %d biased: %d", i, c)
		}
	}
}
