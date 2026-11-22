package clock

import (
	"math"
	"testing"
)

func TestTicksToMSRounding(t *testing.T) {
	cases := []struct {
		ticks     uint64
		timescale uint32
		want      int64
	}{
		{0, 90000, 0},
		{3600, 90000, 40},
		{180000, 90000, 2000},
		{1024, 44100, 23}, // 23.219...
		{512, 44100, 12},  // 11.609... -> 12
		{1, 2, 500},       // exactly half rounds up
		{1, 3, 333},       // 333.33 -> 333
		{2, 3, 667},       // 666.67 -> 667
		{math.MaxUint64, 1, math.MaxInt64},
	}
	for _, c := range cases {
		if got := TicksToMS(c.ticks, c.timescale); got != c.want {
			t.Errorf("TicksToMS(%d,%d)=%d want %d", c.ticks, c.timescale, got, c.want)
		}
	}
}

func TestTicksToMSZeroTimescale(t *testing.T) {
	if got := TicksToMS(12345, 0); got != 0 {
		t.Fatalf("zero timescale = %d, want 0", got)
	}
	if got := TicksToMicros(12345, 0); got != 0 {
		t.Fatalf("zero timescale micros = %d, want 0", got)
	}
	if got := MSToTicks(100, 0); got != 0 {
		t.Fatalf("zero timescale ticks = %d, want 0", got)
	}
}

func TestTicksToMicros(t *testing.T) {
	if got := TicksToMicros(1, 3); got != 333333 {
		t.Fatalf("micros=%d want 333333", got)
	}
	if got := TicksToMicros(0, 1000); got != 0 {
		t.Fatalf("micros=%d want 0", got)
	}
}

func TestRoundTripSegmentDurations(t *testing.T) {
	for _, ts := range []uint32{600, 90000, 44100, 1000} {
		for _, ms := range []int64{0, 1, 23, 1000, 2000, 4000} {
			ticks := MSToTicks(ms, ts)
			got := TicksToMS(ticks, ts)
			if diff := AbsDiff(got, ms); diff > 1 {
				t.Errorf("ts=%d ms=%d roundtrip=%d diff=%d", ts, ms, got, diff)
			}
		}
	}
}

func TestMSToTicksRejectsNegative(t *testing.T) {
	if got := MSToTicks(-5, 1000); got != 0 {
		t.Fatalf("negative ms = %d, want 0", got)
	}
}

func TestSegmentIndexAndStart(t *testing.T) {
	cases := []struct {
		ms, seg int64
		idx     int
		start   int64
	}{
		{0, 2000, 0, 0},
		{-5, 2000, 0, 0},
		{1999, 2000, 0, 0},
		{2000, 2000, 1, 2000},
		{4100, 2000, 2, 4000},
		{100, 0, 0, 0},
	}
	for _, c := range cases {
		if got := SegmentIndex(c.ms, c.seg); got != c.idx {
			t.Errorf("SegmentIndex(%d,%d)=%d want %d", c.ms, c.seg, got, c.idx)
		}
		if got := SegmentStart(c.idx, c.seg); got != c.start {
			t.Errorf("SegmentStart(%d,%d)=%d want %d", c.idx, c.seg, got, c.start)
		}
	}
}

func TestWithinToleranceAndOverflowSafety(t *testing.T) {
	if !WithinTolerance(1000, 1001, 1) {
		t.Fatal("expected aligned")
	}
	if WithinTolerance(1000, 1002, 1) {
		t.Fatal("expected misaligned")
	}
	if got := AbsDiff(math.MaxInt64, math.MinInt64); got != math.MaxInt64 {
		t.Fatalf("overflow AbsDiff = %d", got)
	}
	if got := AddMS(math.MaxInt64, 1); got != math.MaxInt64 {
		t.Fatalf("AddMS overflow = %d", got)
	}
	if got := AddMS(math.MinInt64, -1); got != math.MinInt64 {
		t.Fatalf("AddMS underflow = %d", got)
	}
}

func TestRTPTimestampWrap(t *testing.T) {
	if got := RTPTimestamp(0, 90000); got != 0 {
		t.Fatalf("t=0 -> %d", got)
	}
	if got := RTPTimestamp(1000, 90000); got != 90000 {
		t.Fatalf("t=1s -> %d", got)
	}
	if got := RTPTimestamp(-1, 90000); got != 0 {
		t.Fatalf("negative -> %d", got)
	}
	// 2^32 / 90000 seconds violates the 32-bit clock; wrapping must be exact.
	ms := int64(1) << 40
	a := RTPTimestamp(ms, 90000)
	b := RTPTimestamp(ms+1, 90000)
	if d := RTPDiff(b, a); d != 90 {
		t.Fatalf("1ms delta at 90kHz = %d, want 90", d)
	}
}

func TestRTPDeltaMS(t *testing.T) {
	if got := RTPDeltaMS(90, 90000); got != 1 {
		t.Fatalf("90@90k = %d, want 1", got)
	}
	if got := RTPDeltaMS(0, 0); got != 0 {
		t.Fatalf("zero rate = %d", got)
	}
}

func TestBaseValidation(t *testing.T) {
	if _, err := NewBase(0); err == nil {
		t.Fatal("expected error for zero timescale")
	}
	b, err := NewBase(90000)
	if err != nil || !b.Valid() {
		t.Fatalf("NewBase: %v valid=%v", err, b.Valid())
	}
	if got := b.DurationMS(180000); got != 2000 {
		t.Fatalf("DurationMS=%d", got)
	}
	if got := b.Ticks(2000); got != 180000 {
		t.Fatalf("Ticks=%d", got)
	}
}
