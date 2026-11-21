package split

import (
	"testing"
)

func model(start uint64, durs ...uint32) FragModel {
	return FragModel{StartUnits: start, Samples: durs}
}

func TestRoundToNearestSampleBoundary(t *testing.T) {
	// Samples of 100 units at 1000 Hz → 100 ms each.
	c, err := NewCutter(1000, []FragModel{model(0, 100, 100, 100, 100)})
	if err != nil {
		t.Fatal(err)
	}
	units, ms, err := c.Round(130)
	if err != nil {
		t.Fatal(err)
	}
	if units != 100 || ms != 100 {
		t.Fatalf("Round(130) = (%d, %v), want (100, 100)", units, ms)
	}
	units, _, _ = c.Round(160)
	if units != 200 {
		t.Fatalf("Round(160) units=%d want 200", units)
	}
}

func TestRoundTieGoesToEvenIndex(t *testing.T) {
	// Boundaries at 0,100,200,300 (indices 0..3). Cut at 150 is an
	// exact tie between index 1 and 2: round half to even → index 2.
	c, _ := NewCutter(1000, []FragModel{model(0, 100, 100, 100)})
	units, _, err := c.Round(150)
	if err != nil {
		t.Fatal(err)
	}
	if units != 200 {
		t.Fatalf("tie at 150: units=%d want 200 (even index)", units)
	}
	// Cut at 250: tie between index 2 and 3 → index 2.
	units, _, _ = c.Round(250)
	if units != 200 {
		t.Fatalf("tie at 250: units=%d want 200 (even index)", units)
	}
}

func TestRoundClampsToAvailableBoundaries(t *testing.T) {
	c, _ := NewCutter(1000, []FragModel{model(100, 100, 100)})
	units, _, _ := c.Round(0) // before the first boundary
	if units != 100 {
		t.Fatalf("units=%d want 100 (first boundary)", units)
	}
	units, _, _ = c.Round(10000) // beyond the last
	if units != 300 {
		t.Fatalf("units=%d want 300 (last boundary)", units)
	}
}

func TestCutReportsActualOffsets(t *testing.T) {
	// 33.3 ms samples (3000 units at 90 kHz) over 1 s.
	var samples []uint32
	for i := 0; i < 30; i++ {
		samples = append(samples, 3000)
	}
	c, _ := NewCutter(90000, []FragModel{model(0, samples...)})
	segs, err := Cut("video", c, 1000, 400)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 3 {
		t.Fatalf("segments=%d want 3", len(segs))
	}
	// Requested 400 ms → nearest boundary 396.666.. ms? No: 3000
	// units = 33.333 ms; 400 ms = 36000 units = exactly 12 samples.
	if segs[0].ReqStartMs != 0 || segs[0].ReqEndMs != 400 {
		t.Fatalf("requested range not preserved: %+v", segs[0])
	}
	if segs[0].ActEndMs != 400 {
		t.Fatalf("actual end=%v want 400", segs[0].ActEndMs)
	}
	// 800 ms = 72000 units = 24 samples exactly.
	if segs[1].ActEndMs != 800 {
		t.Fatalf("actual end=%v want 800", segs[1].ActEndMs)
	}
}

func TestCutRejectsInvalidRanges(t *testing.T) {
	c, _ := NewCutter(1000, []FragModel{model(0, 100)})
	if _, err := Cut("v", c, 0, 100); err == nil {
		t.Fatal("expected error for zero total")
	}
	if _, err := Cut("v", c, 100, 0); err == nil {
		t.Fatal("expected error for zero interval")
	}
}

func TestNewCutterOverflow(t *testing.T) {
	_, err := NewCutter(1000, []FragModel{model(^uint64(0)-5, 100)})
	if err == nil {
		t.Fatal("expected overflow error")
	}
}
