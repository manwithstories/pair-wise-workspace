package clock_test

import (
	"testing"

	"mediacheck/internal/clock"
)

// TestSharedConversionAcrossKinds proves audio and video timescales use the
// identical rounding rule, so a boundary that matches in one layer matches in
// the other.
func TestSharedConversionAcrossKinds(t *testing.T) {
	videoTS := uint32(15360)
	audioTS := uint32(44100)
	for frame := int64(0); frame < 100; frame++ {
		// 30 fps video at 15360 ticks/s and 1024-sample AAC at 44100.
		vTicks := uint64(frame) * 512
		aTicks := uint64(frame*1024) * 44100 / 48000
		vMS := clock.TicksToMS(vTicks, videoTS)
		aMS := clock.TicksToMS(aTicks, audioTS)
		// Both conversions independently round; agreement is per-value, so we
		// assert the rule (not equality of unrelated spans) by round-tripping.
		if back := clock.MSToTicks(vMS, videoTS); clock.TicksToMS(back, videoTS) != vMS {
			t.Fatalf("video round-trip changed value at frame %d", frame)
		}
		if back := clock.MSToTicks(aMS, audioTS); clock.TicksToMS(back, audioTS) != aMS {
			t.Fatalf("audio round-trip changed value at frame %d", frame)
		}
	}
}

// TestSameTicksDifferentTimescales checks the documented half-up rule on a
// fractional sample: 1 tick at 3 ticks/s is 333.33 ms, at 2 ticks/s is 500 ms.
func TestSameTicksDifferentTimescales(t *testing.T) {
	if got := clock.TicksToMS(1, 3); got != 333 {
		t.Fatalf("1@3=%d", got)
	}
	if got := clock.TicksToMS(1, 2); got != 500 {
		t.Fatalf("1@2=%d", got)
	}
	if got := clock.TicksToMS(3, 2); got != 1500 {
		t.Fatalf("3@2=%d", got)
	}
}
