package sync

import (
	"testing"

	"github.com/example/mediad/internal/clock"
	"github.com/example/mediad/internal/model"
)

func vPoints(times ...int64) []Point {
	out := make([]Point, len(times))
	for i, tm := range times {
		out[i] = Point{TrackID: 1, Time: clock.Micros(tm)}
	}
	return out
}

func aPoints(times ...int64) []Point {
	out := make([]Point, len(times))
	for i, tm := range times {
		out[i] = Point{TrackID: 2, Time: clock.Micros(tm), IsAudio: true}
	}
	return out
}

func TestEstimateOffset(t *testing.T) {
	// audio consistently 15 ms behind video
	off, err := Estimate(
		vPoints(0, 33333, 66666),
		aPoints(15000, 48333, 81666),
	)
	if err != nil {
		t.Fatal(err)
	}
	if off != 15000 {
		t.Fatalf("offset = %d, want 15000", off)
	}
	if ms := OffsetMillis(off); ms != 15 {
		t.Fatalf("ms = %d", ms)
	}
}

func TestEstimateRejectsEmpty(t *testing.T) {
	if _, err := Estimate(nil, aPoints(1)); err == nil {
		t.Fatal("empty video must error")
	}
}

func TestAlignWithinTolerance(t *testing.T) {
	cfg := DefaultConfig()
	units := append(vPoints(0, 33333), aPoints(15000, 48333)...)
	aligned := Align(units, 15000, cfg)
	var audioUnits []AlignedUnit
	for _, a := range aligned {
		if a.IsAudio {
			audioUnits = append(audioUnits, a)
		}
	}
	if len(audioUnits) != 2 {
		t.Fatalf("audio units = %d", len(audioUnits))
	}
	for _, a := range audioUnits {
		if a.Decision != DecisionAligned || !a.WithinTol {
			t.Fatalf("unit not aligned: %+v", a)
		}
		if a.Corrected != a.Original-15000 {
			t.Fatalf("correction wrong: %+v", a)
		}
	}
	if err := RejectOutOfTolerance(aligned); err != nil {
		t.Fatalf("in-tolerance units must pass: %v", err)
	}
}

func TestAlignOutOfToleranceRetains(t *testing.T) {
	cfg := DefaultConfig() // tolerance 20ms
	units := append(vPoints(0), aPoints(50000)...)
	aligned := Align(units, 50000, cfg)

	// The audio unit must still be present — never silently dropped.
	var audio *AlignedUnit
	for i := range aligned {
		if aligned[i].IsAudio {
			audio = &aligned[i]
		}
	}
	if audio == nil {
		t.Fatal("out-of-tolerance audio unit was dropped")
	}
	if audio.Decision != DecisionTolerance || audio.WithinTol {
		t.Fatalf("decision = %+v", audio)
	}
	err := RejectOutOfTolerance(aligned)
	me, ok := model.AsError(err)
	if !ok || me.Kind != model.KindConflict {
		t.Fatalf("expected conflict error, got %v", err)
	}
}

func TestAlignShiftBounded(t *testing.T) {
	cfg := Config{ToleranceMicros: 1_000_000, MaxShiftMicros: 20_000}
	units := aPoints(100000)
	aligned := Align(units, 100000, cfg)
	if aligned[0].Shift != -20000 {
		t.Fatalf("shift = %d, must clamp to -20000", aligned[0].Shift)
	}
	if aligned[0].Corrected != 80000 {
		t.Fatalf("corrected = %d", aligned[0].Corrected)
	}
}

func TestNoUnitsLost(t *testing.T) {
	cfg := DefaultConfig()
	v := vPoints(0, 33333, 66666)
	a := aPoints(15000, 48333)
	units := append(v, a...)
	aligned := Align(units, 15000, cfg)
	if len(aligned) != len(units) {
		t.Fatalf("lost units: in=%d out=%d", len(units), len(aligned))
	}
}
