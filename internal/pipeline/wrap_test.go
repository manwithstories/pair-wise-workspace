package pipeline

import (
	"testing"

	"archive.local/recover/internal/clock"
	"archive.local/recover/internal/model"
	"archive.local/recover/internal/testkit"
)

// TestWrapVFRDriftBreakIsLocated checks that the drift fit is split at the
// actual clock jump rather than at a following anchor, and that samples on
// both sides remain within the sync budget.
func TestWrapVFRDriftBreakIsLocated(t *testing.T) {
	fx, err := testkit.Generate(testkit.Options{
		Seed: 17, DurationNs: 60e9, Bitrates: []int{800}, Loss: 0.03, Case: testkit.CaseWrapVFRDrift,
	})
	if err != nil {
		t.Fatal(err)
	}
	rd := &fx.Renditions[0]
	r, err := clock.Unwrap(clock.Input{
		TrackID: 1, Kind: model.KindVideo, Timescale: rd.VideoTimescale,
		RawPTS: rd.Clock, Anchors: rd.Anchors, BreakNs: 20e6,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Breakpoints) == 0 {
		t.Fatal("jump not segmented")
	}
	// The generator injects the +0.5 s reset at 60% of the capture.
	want := len(rd.Clock) * 6 / 10
	got := r.Breakpoints[0]
	if d := got - want; d < -25 || d > 25 {
		t.Fatalf("breakpoint %d, want near %d (cadence rounding allowed)", got, want)
	}
	for i := range r.Track.PTS {
		if d := r.Track.PTS[i] - rd.ProgramNs[i]; d > 20_000_000 || d < -20_000_000 {
			t.Fatalf("sample %d off by %d ns", i, d)
		}
	}
}
