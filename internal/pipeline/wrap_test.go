package pipeline

import (
	"testing"

	"archive.local/recover/internal/clock"
	"archive.local/recover/internal/model"
	"archive.local/recover/internal/testkit"
)

// TestShortWrapCaseHasAnchorsOnBothSides guards the fixture property the
// drift fit relies on: even a 20 s capture must carry anchors after an injected
// clock break, otherwise the break cannot be segmented and the mapping is
// extrapolated (which is what produced a 500 ms sync error before).
func TestShortWrapCaseHasAnchorsOnBothSides(t *testing.T) {
	fx, err := testkit.Generate(testkit.Options{
		Seed: 99, DurationNs: 20e9, Bitrates: []int{800}, Loss: 0.03, Case: testkit.CaseWrapVFRDrift,
	})
	if err != nil {
		t.Fatal(err)
	}
	rd := &fx.Renditions[0]
	breakAt := len(rd.Clock) * 6 / 10
	after := 0
	for _, a := range rd.Anchors {
		if a.Sample > breakAt {
			after++
		}
	}
	if after == 0 {
		t.Fatalf("no anchor after the clock break at sample %d: %d anchors total", breakAt, len(rd.Anchors))
	}
	r, err := clock.Unwrap(clock.Input{
		TrackID: 1, Kind: model.KindVideo, Timescale: rd.VideoTimescale,
		RawPTS: rd.Clock, Anchors: rd.Anchors, BreakNs: 20e6,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Breakpoints) == 0 {
		t.Fatal("clock break not segmented on a short capture")
	}
	for i := range r.Track.PTS {
		if d := r.Track.PTS[i] - rd.ProgramNs[i]; d > 20_000_000 || d < -20_000_000 {
			t.Fatalf("sample %d off by %d ns", i, d)
		}
	}
}
