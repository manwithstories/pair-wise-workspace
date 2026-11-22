package clock

import (
	"errors"
	"math"
	"testing"
)

func TestScaleNanosRoundsHalfAwayFromZero(t *testing.T) {
	s := MustScale(90000)
	for _, tc := range []struct{ units, want int64 }{
		{0, 0},
		{90000, NSPerSecond},
		{-90000, -NSPerSecond},
		{1, 11111},   // 11111.11 rounds down
		{2, 22222},   // 22222.22 rounds down
		{9, 100000},  // 100000 exactly
		{5, 55556},   // 55555.55 rounds up
		{-5, -55556}, // symmetry
	} {
		got, err := s.Nanos(tc.units)
		if err != nil {
			t.Fatalf("Nanos(%d): %v", tc.units, err)
		}
		if got != tc.want {
			t.Fatalf("Nanos(%d) = %d, want %d", tc.units, got, tc.want)
		}
	}
}

func TestScaleUnitsInverse(t *testing.T) {
	s := MustScale(48000)
	for _, ns := range []int64{0, 1, 20_833, NSPerSecond, -NSPerSecond, 3600 * NSPerSecond} {
		u, err := s.Units(ns)
		if err != nil {
			t.Fatalf("Units(%d): %v", ns, err)
		}
		back, err := s.Nanos(u)
		if err != nil {
			t.Fatalf("Nanos(%d): %v", u, err)
		}
		if diff := back - ns; diff > 1000 || diff < -1000 {
			t.Fatalf("Units/Nanos round trip drift %d for %d", diff, ns)
		}
	}
}

func TestScaleRejectsZero(t *testing.T) {
	if _, err := NewScale(0); !errors.Is(err, ErrTimescale) {
		t.Fatalf("want ErrTimescale, got %v", err)
	}
	var zero Scale
	if _, err := zero.Nanos(1); !errors.Is(err, ErrTimescale) {
		t.Fatalf("want ErrTimescale, got %v", err)
	}
}

func TestScaleOverflow(t *testing.T) {
	s := MustScale(1)
	if _, err := s.Nanos(math.MaxInt64); !errors.Is(err, ErrOverflow) {
		t.Fatalf("want ErrOverflow, got %v", err)
	}
	if _, err := s.Nanos(math.MinInt64); !errors.Is(err, ErrOverflow) {
		t.Fatalf("want ErrOverflow, got %v", err)
	}
}

func TestSignedDelta32Unwraps(t *testing.T) {
	cases := []struct {
		prev, cur uint32
		want      int64
	}{
		{0, 10, 10},
		{math.MaxUint32 - 2, 3, 6}, // wraps forward
		{3, math.MaxUint32 - 2, -6},
	}
	for _, tc := range cases {
		if got := SignedDelta32(tc.prev, tc.cur); got != tc.want {
			t.Fatalf("SignedDelta32(%d,%d)=%d want %d", tc.prev, tc.cur, got, tc.want)
		}
	}
}

func TestTimelineOrigin(t *testing.T) {
	tl := NewTimeline(5 * NSPerSecond)
	if got := tl.At(7 * NSPerSecond); got != 2*NSPerSecond {
		t.Fatalf("At = %d", got)
	}
	if got := tl.At(1 * NSPerSecond); got != -4*NSPerSecond {
		t.Fatalf("negative At = %d", got)
	}
}

func TestValidateLimits(t *testing.T) {
	if err := ValidateDuration(MaxAssetDurationNanos); err != nil {
		t.Fatal(err)
	}
	if err := ValidateDuration(MaxAssetDurationNanos + 1); err == nil {
		t.Fatal("want duration rejection")
	}
	if err := ValidatePacketCount(MaxPacketsPerTrack); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePacketCount(MaxPacketsPerTrack + 1); err == nil {
		t.Fatal("want packet rejection")
	}
}

func TestBudget(t *testing.T) {
	b := NewBudget(10)
	if err := b.Reserve(6); err != nil {
		t.Fatal(err)
	}
	if err := b.Reserve(6); err == nil {
		t.Fatal("want over-budget rejection")
	}
	b.Release(6)
	if b.Used() != 0 {
		t.Fatalf("used = %d", b.Used())
	}
	b.Release(100)
	if b.Used() != 0 {
		t.Fatalf("used should clamp at zero, got %d", b.Used())
	}
	if err := b.Reserve(-1); err == nil {
		t.Fatal("want negative reservation rejection")
	}
}
