package clock

import (
	"math"
	"testing"

	"medpipe/internal/model"
)

func makeFrames(tr model.Track, n int, stepTicks float64, startTick uint64) []model.Frame {
	out := make([]model.Frame, n)
	mod := uint64(1) << tr.ClockBits
	for i := 0; i < n; i++ {
		tick := startTick + uint64(math.Round(float64(i)*stepTicks))
		out[i] = model.Frame{
			TrackID:  tr.ID,
			Seq:      uint64(i),
			RawTS:    tick % mod,
			Keyframe: i%10 == 0,
			Payload:  []byte{byte(i)},
		}
	}
	return out
}

func TestBuildTimelineDeterministicUnderDriftAndVFR(t *testing.T) {
	tr := model.Track{ID: "cam", Kind: model.KindVideo, Timescale: 90000, ClockBits: 16, NominalTickDelta: 3000, OriginNS: 500_000_000}
	frames := makeFrames(tr, 200, 3000.6, 64000) // 200ppm slow
	// Perturb every fifth frame like a VFR capture: longer gap before it.
	for i := 5; i < len(frames); i += 5 {
		frames[i].RawTS = (frames[i].RawTS + 1500) % 65536
	}
	tl1, err := Build([]model.Track{tr}, map[string][]model.Frame{"cam": frames}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	tl2, err := Build([]model.Track{tr}, map[string][]model.Frame{"cam": frames}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	tt1, _ := tl1.Track("cam")
	tt2, _ := tl2.Track("cam")
	if !tt1.VFR {
		t.Error("timeline should be VFR")
	}
	if tt1.DriftPPM < 100 {
		t.Errorf("drift = %v ppm, want ~200", tt1.DriftPPM)
	}
	for i := range tt1.Frames {
		if tt1.Frames[i].NS != tt2.Frames[i].NS || tt1.Frames[i].IntervalNS != tt2.Frames[i].IntervalNS {
			t.Fatalf("frame %d not deterministic: %+v vs %+v", i, tt1.Frames[i], tt2.Frames[i])
		}
	}
	samples := map[int64]bool{}
	for i := 0; i < len(tt1.Frames)-1; i++ {
		samples[tt1.Frames[i].IntervalNS] = true
	}
	if len(samples) < 2 {
		t.Errorf("expected more than one distinct interval, got %v", samples)
	}
	if tt1.Frames[0].NS != 500_000_000 {
		t.Errorf("start NS = %d, want origin 500ms", tt1.Frames[0].NS)
	}
}

func TestBuildRejectsAmbiguousWrap(t *testing.T) {
	tr := model.Track{ID: "cam", Kind: model.KindVideo, Timescale: 90000, ClockBits: 16, NominalTickDelta: 3000}
	frames := []model.Frame{
		{TrackID: "cam", Seq: 0, RawTS: 1000},
		{TrackID: "cam", Seq: 1, RawTS: 40000},
	}
	_, err := Build([]model.Track{tr}, map[string][]model.Frame{"cam": frames}, Options{})
	if !model.IsKind(err, model.KindAmbiguous) {
		t.Fatalf("got %v, want ambiguity", err)
	}
}

func TestBuildRejectsBadTracks(t *testing.T) {
	tr := model.Track{ID: "cam", Timescale: 90000}
	if _, err := Build(nil, nil, Options{}); err == nil {
		t.Fatal("expected error for no tracks")
	}
	if _, err := Build([]model.Track{tr}, nil, Options{}); err == nil {
		t.Fatal("expected error for missing frame slice")
	}
	tr.Timescale = 0
	if _, err := Build([]model.Track{tr}, map[string][]model.Frame{"cam": nil}, Options{}); err == nil {
		t.Fatal("expected error for zero timescale")
	}
	if _, err := Build([]model.Track{{ID: "a", Timescale: 1}, {ID: "a", Timescale: 1}},
		map[string][]model.Frame{"a": nil}, Options{}); err == nil {
		t.Fatal("expected error for duplicate track")
	}
}

func TestBuildRejectsNonAdvancingSharedTimeline(t *testing.T) {
	// 64-bit timestamps differing by less than a tick at the timescale
	// round to the same nanosecond.
	tr := model.Track{ID: "a", Timescale: 4_000_000_000, ClockBits: 64, NominalTickDelta: 1}
	frames := []model.Frame{{TrackID: "a", Seq: 0, RawTS: 0}, {TrackID: "a", Seq: 1, RawTS: 1}}
	if _, err := Build([]model.Track{tr}, map[string][]model.Frame{"a": frames}, Options{}); err == nil {
		t.Fatal("expected non-advancing timeline error")
	}
}

func TestBuildEmptyTrackTimeline(t *testing.T) {
	tr := model.Track{ID: "a", Kind: model.KindAudio, Timescale: 48000, OriginNS: 1000}
	tl, err := Build([]model.Track{tr}, map[string][]model.Frame{"a": nil}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	tt, _ := tl.Track("a")
	if len(tt.Frames) != 0 || tt.StartNS != 1000 || tt.EndNS != 1000 {
		t.Fatalf("empty track = %+v", tt)
	}
}
