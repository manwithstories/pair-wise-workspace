package timeline

import (
	"math"
	"testing"

	"archive/internal/manifest"
	"archive/internal/media"
)

var loc = Loc{Track: "v1", Offset: 7}

func TestTicksToMicrosRoundTrip(t *testing.T) {
	// 90000Hz: 1 tick = 11.111...µs; round trip must stay within 1 tick.
	for _, ticks := range []int64{0, 1, 9000, 90000, 1 << 40} {
		us, err := TicksToMicros(ticks, 90000, loc)
		if err != nil {
			t.Fatal(err)
		}
		back, err := MicrosToTicks(us, 90000, loc)
		if err != nil {
			t.Fatal(err)
		}
		if d := back - ticks; d < -1 || d > 1 {
			t.Fatalf("ticks=%d round trip off by %d", ticks, d)
		}
	}
}

func TestConversionErrorUnderOneMilli(t *testing.T) {
	// Worst-case rounding: error must be <= 0.5µs, far below 1ms.
	us, _ := TicksToMicros(48038, 48000, loc) // 1000791.66...µs
	if us != 1000792 {
		t.Fatalf("got %d want 1000792", us)
	}
}

func TestMultiplyOverflowReported(t *testing.T) {
	_, err := TicksToMicros(math.MaxInt64-1, 90000, loc)
	me, ok := err.(*media.Error)
	if !ok {
		t.Fatalf("expected *media.Error, got %T", err)
	}
	if me.Track != "v1" || me.Offset != 7 {
		t.Fatalf("overflow error missing track/offset: %v", me)
	}
}

func TestEditListMapping(t *testing.T) {
	edits := []manifest.RawEdit{
		{MediaTime: -1, SegmentDuration: 100000}, // 100ms dwell @1e6
		{MediaTime: 9000, SegmentDuration: 900000},
	}
	// Media tick 9000 (100ms @90k) maps to program 100ms(dwell)+0.
	us, err := MediaToProgram(9000, 90000, edits, 1_000_000, loc)
	if err != nil {
		t.Fatal(err)
	}
	if us != 100000 {
		t.Fatalf("got %d want 100000", us)
	}
	// Media tick 99000 -> dwell + 1s = 1.1s.
	us, _ = MediaToProgram(99000, 90000, edits, 1_000_000, loc)
	if us != 1_100_000 {
		t.Fatalf("got %d want 1100000", us)
	}
	// Before first covering edit -> out of range.
	if _, err := MediaToProgram(8999, 90000, edits, 1_000_000, loc); err == nil {
		t.Fatal("expected out-of-range error before first edit")
	}
	// Beyond last edit is fine (entry covers open-ended media time);
	// EditEnd validates the declared coverage instead.
	end, err := EditEnd(edits, 90000, 1_000_000, loc)
	if err != nil {
		t.Fatal(err)
	}
	if end != 9000+81000 {
		t.Fatalf("EditEnd=%d want 90000", end)
	}
}

func TestNoEditListIdentity(t *testing.T) {
	us, err := MediaToProgram(48000, 48000, nil, 1_000_000, loc)
	if err != nil || us != 1_000_000 {
		t.Fatalf("got %d, %v", us, err)
	}
}
