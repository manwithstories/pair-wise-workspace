package media

import (
	"math"
	"testing"
)

func TestClockConversions(t *testing.T) {
	if got := Duration90k(1000); got != Clock90k {
		t.Fatalf("1s = %d ticks, want %d", got, Clock90k)
	}
	if got := ToMillis(Clock90k); got != 1000 {
		t.Fatalf("90k ticks = %d ms, want 1000", got)
	}
	// 48 kHz: one second of samples maps to exactly 90,000 ticks.
	if got := FromSamples(48_000, 48_000); got != Clock90k {
		t.Fatalf("48k samples = %d ticks", got)
	}
	// 16 kHz round trip at half a second.
	half := FromSamples(8_000, 16_000)
	if got := ToSamples(half, 16_000); got != 8_000 {
		t.Fatalf("round trip = %d samples, want 8000", got)
	}
}

func TestFractionalStepMonotone(t *testing.T) {
	// 23.976-like rate must accumulate without drift beyond one interval.
	f := NewFractionalStep(24)
	var total uint64
	for i := 0; i < 24; i++ {
		total += f.Next()
	}
	if total != Clock90k {
		t.Fatalf("24 frames span %d ticks, want %d", total, Clock90k)
	}
}

func TestSyncDiffSafe(t *testing.T) {
	if got := SyncDiffMillis(0, Clock90k); got != 1000 {
		t.Fatalf("diff = %d ms, want 1000", got)
	}
	if got := SyncDiffMillis(Clock90k, 0); got != 1000 {
		t.Fatalf("reversed diff = %d ms, want 1000", got)
	}
}

func TestSafeMath(t *testing.T) {
	if _, ok := AddU64(math.MaxUint64, 1); !ok {
		t.Fatal("expected overflow on max+1")
	}
	if _, ok := MulU64(math.MaxUint64, 2); !ok {
		t.Fatal("expected overflow on max*2")
	}
	if v, ok := MulU64(123, 0); ok || v != 0 {
		t.Fatal("zero multiply must be 0, no overflow")
	}
	if CheckBounds(10, 5, 15) != true {
		t.Fatal("in-bounds read rejected")
	}
	if CheckBounds(10, 6, 15) != false {
		t.Fatal("out-of-bounds read accepted")
	}
	if CheckBounds(math.MaxUint64, 2, 100) != false {
		t.Fatal("overflowing bounds check accepted")
	}
}
