package rng

import (
	"math"
	"testing"
)

func TestDeterministic(t *testing.T) {
	a, b := New(42), New(42)
	for i := 0; i < 1000; i++ {
		if a.Uint64() != b.Uint64() {
			t.Fatalf("diverged at %d", i)
		}
	}
	if New(42).Uint64() == New(43).Uint64() {
		t.Fatal("different seeds collided")
	}
}

func TestRanges(t *testing.T) {
	s := New(7)
	for i := 0; i < 10000; i++ {
		f := s.Float()
		if f < 0 || f >= 1 {
			t.Fatalf("float out of range: %v", f)
		}
		sym := s.Symmetric()
		if sym < -1 || sym >= 1 {
			t.Fatalf("symmetric out of range: %v", sym)
		}
		if n := s.Intn(10); n < 0 || n >= 10 {
			t.Fatalf("intn out of range: %d", n)
		}
	}
	if s.Intn(0) != 0 || s.Intn(-3) != 0 {
		t.Fatal("degenerate Intn")
	}
}

func TestChanceMean(t *testing.T) {
	s := New(99)
	hits := 0
	const n = 100000
	for i := 0; i < n; i++ {
		if s.Chance(0.08) {
			hits++
		}
	}
	if math.Abs(float64(hits)/n-0.08) > 0.005 {
		t.Fatalf("mean %.4f far from 0.08", float64(hits)/n)
	}
	if s.Chance(0) || !s.Chance(1) {
		t.Fatal("boundary probability wrong")
	}
}
