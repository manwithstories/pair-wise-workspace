package clock

import (
	"errors"
	"math"
	"testing"
)

func TestScaleToTicks(t *testing.T) {
	cases := []struct {
		value     int64
		timescale uint32
		want      Tick
	}{
		{0, 1000, 0},
		{1000, 1000, TickRate},
		{1, 90000, 1},
		{48000, 48000, TickRate},
		{16000, 16000, TickRate}, // exactly one second
		{-1000, 1000, -TickRate},
		{-16000, 16000, -TickRate},
	}
	for _, c := range cases {
		got, err := ScaleToTicks(c.value, c.timescale)
		if err != nil {
			t.Fatalf("ScaleToTicks(%d,%d): %v", c.value, c.timescale, err)
		}
		if got != c.want {
			t.Fatalf("ScaleToTicks(%d,%d)=%d want %d", c.value, c.timescale, got, c.want)
		}
	}
}

func TestScaleRejects(t *testing.T) {
	if _, err := ScaleToTicks(1, 0); !errors.Is(err, ErrTimescale) {
		t.Fatalf("zero timescale: %v", err)
	}
	if _, err := ScaleToTicks(math.MaxInt64, 1); !errors.Is(err, ErrOverflow) {
		t.Fatalf("overflow: %v", err)
	}
	if _, err := ScaleToTicks(math.MinInt64, 1000); !errors.Is(err, ErrOverflow) {
		t.Fatalf("negative overflow: %v", err)
	}
}

func TestRoundTripUnits(t *testing.T) {
	for _, ts := range []uint32{1000, 90000, 48000, 25} {
		for _, v := range []int64{0, 1, -1, 12345, -98765} {
			tick, err := ScaleToTicks(v, ts)
			if err != nil {
				t.Fatal(err)
			}
			back, err := TicksToUnits(tick, ts)
			if err != nil {
				t.Fatal(err)
			}
			// Flooring loses at most one unit on the way down and one on
			// the way back.
			if diff := back - v; diff > 1 || diff < -1 {
				t.Fatalf("roundtrip %d@%d -> %d -> %d", v, ts, tick, back)
			}
		}
	}
}

func TestAddSubOverflow(t *testing.T) {
	if _, err := Add(Tick(math.MaxInt64), 1); !errors.Is(err, ErrOverflow) {
		t.Fatalf("add overflow: %v", err)
	}
	if _, err := Add(Tick(math.MinInt64), -1); !errors.Is(err, ErrOverflow) {
		t.Fatalf("sub underflow: %v", err)
	}
	if _, err := Sub(Tick(math.MinInt64), Tick(math.MaxInt64)); !errors.Is(err, ErrOverflow) {
		t.Fatalf("sub overflow: %v", err)
	}
	if got, err := Sub(100, -5); err != nil || got != 105 {
		t.Fatalf("sub negative: %d %v", got, err)
	}
}

func TestUnwrapperWrap(t *testing.T) {
	var u Unwrapper
	first, err := u.Extend(0xFFFFFF00)
	if err != nil || first != 0 {
		t.Fatalf("first: %d %v", first, err)
	}
	next, err := u.Extend(0x00000100)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(0x200); next != want {
		t.Fatalf("wrap delta: got %d want %d", next, want)
	}
	prev, err := u.Extend(0x00000101)
	if err != nil || prev != 0x201 {
		t.Fatalf("continue: %d %v", prev, err)
	}
}

func TestUnwrapperRejectsRegression(t *testing.T) {
	var u Unwrapper
	if _, err := u.Extend(1000); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Extend(999); err != nil {
		t.Fatalf("small regression should be tolerated: %v", err)
	}
	var v Unwrapper
	if _, err := v.Extend(0x7FFFFFFF); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Extend(0x100); !errors.Is(err, ErrBadRollbackTick) {
		t.Fatalf("expected rollback error, got %v", err)
	}
}

func TestBaseTicks(t *testing.T) {
	b, err := NewBase(48000)
	if err != nil {
		t.Fatal(err)
	}
	// The first observation establishes the baseline and maps to zero;
	// the next one carries the real delta.
	if tick, err := b.Tick(48000); err != nil || tick != 0 {
		t.Fatalf("baseline: %d %v", tick, err)
	}
	if tick, err := b.Tick(96000); err != nil || tick != TickRate {
		t.Fatalf("Tick: %d %v", tick, err)
	}
	if _, err := NewBase(0); !errors.Is(err, ErrTimescale) {
		t.Fatalf("zero base: %v", err)
	}
	if err := SoundCheck(10, -1); !errors.Is(err, ErrDuration) {
		t.Fatalf("negative duration: %v", err)
	}
	if err := SoundCheck(Tick(math.MaxInt64), 1); !errors.Is(err, ErrOverflow) {
		t.Fatalf("end overflow: %v", err)
	}
}
