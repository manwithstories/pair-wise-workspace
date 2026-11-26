package media_test

import (
	"testing"

	"mediascan/internal/media"
)

func TestScaleTicksExactAndZeroTimescale(t *testing.T) {
	got, ok := media.ScaleTicks(3003, 30000, media.UnifiedTimescale)
	if !ok {
		t.Fatal("expected valid scaling")
	}
	if want := int64(100_100_000); got != want {
		t.Fatalf("scale = %d, want %d", got, want)
	}

	// 4096/48000 second is 85,333,333.333... ns; round to nearest.
	got, ok = media.ScaleTicks(4096, 48000, media.UnifiedTimescale)
	if !ok || got != 85_333_333 {
		t.Fatalf("rounded scale = %d ok=%v, want 85333333 true", got, ok)
	}

	if _, ok := media.ScaleTicks(10, 0, media.UnifiedTimescale); ok {
		t.Fatal("zero source timescale must be invalid")
	}
	if _, ok := media.ScaleTicks(10, 48000, 0); ok {
		t.Fatal("zero destination timescale must be invalid")
	}
}

func TestIntervalAndRational(t *testing.T) {
	start := media.NewTime(100, 1000)
	end := media.NewTime(350, 1000)
	iv := media.NewInterval(start, end)
	if iv.Ticks != 250 || iv.Timescale != 1000 {
		t.Fatalf("interval = %+v", iv)
	}
	if r := end.Rational(); r.Num != 350 || r.Den != 1000 {
		t.Fatalf("rational = %+v", r)
	}
}
