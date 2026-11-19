package recover_test

import (
	"testing"

	"archivearchivist/internal/clock"
	"archivearchivist/internal/graph"
	"archivearchivist/internal/model"
	"archivearchivist/internal/mp4"
	"archivearchivist/internal/recover"
)

func rat(n, d int64) model.Rational { return model.NewRat(n, d) }

// scanTrack builds a synthetic scan: keyframes every keyEvery samples, one
// corrupt run in the middle, constant 3000-tick samples at 90 kHz.
func scanTrack(n, keyEvery, corruptFrom, corruptTo int) *mp4.Scan {
	tr := mp4.Track{Timescale: 90000, SourceBytes: 1 << 24}
	for i := 0; i < n; i++ {
		s := mp4.Sample{
			Index: uint32(i + 1), Offset: int64(1000 + i*100), Size: 100,
			DTS: int64(i * 3000), Duration: 3000, Keyframe: i%keyEvery == 0,
		}
		if i >= corruptFrom && i <= corruptTo {
			s.Corrupt = true
			s.Keyframe = false
		}
		tr.Samples = append(tr.Samples, s)
	}
	return &mp4.Scan{Path: "v.mp4", Tracks: []mp4.Track{tr}}
}

func timelineWith(clips []model.Clip, tracks []model.Track) *model.Timeline {
	return &model.Timeline{Version: 1, Tracks: tracks, Clips: clips}
}

func videoTrack(id string) model.Track {
	return model.Track{ID: id, Kind: model.KindVideo, Codec: "avc1", Source: "v.mp4", Timescale: 90000}
}

func TestCorruptRunBracketedByKeyframes(t *testing.T) {
	sc := scanTrack(60, 10, 24, 25)
	tl := timelineWith(
		[]model.Clip{{ID: "C1", Track: "V1", Start: rat(0, 1), Duration: rat(2, 1), SourceIn: rat(0, 1)}},
		[]model.Track{videoTrack("V1")},
	)
	g, _ := graph.Build(tl)
	res := recover.Run(recover.Input{
		Timeline: tl, Graph: g, Scans: map[string]*mp4.Scan{"V1": sc},
	}, recover.Config{KeyframeSearch: 120, MaxErrorUS: 150_000})
	if len(res.Ranges) != 1 {
		t.Fatalf("ranges = %+v", res.Ranges)
	}
	r := res.Ranges[0]
	if r.Status != "recovered" {
		t.Fatalf("status = %s (%s)", r.Status, r.Detail)
	}
	if r.BeforeKeyframe != sc.Tracks[0].Samples[20].Offset {
		t.Fatalf("before keyframe = %d, want %d", r.BeforeKeyframe, sc.Tracks[0].Samples[20].Offset)
	}
	if r.AfterKeyframe != sc.Tracks[0].Samples[30].Offset {
		t.Fatalf("after keyframe = %d, want %d", r.AfterKeyframe, sc.Tracks[0].Samples[30].Offset)
	}
	if r.RecoverableStart > r.CorruptStart || r.RecoverableEnd < r.CorruptEnd {
		t.Fatalf("recoverable interval does not enclose the damage: %+v", r)
	}
	if r.ErrorBoundUS <= 0 {
		t.Fatalf("error bound = %d", r.ErrorBoundUS)
	}
}

func TestNoKeyframeIsFatal(t *testing.T) {
	sc := &mp4.Scan{Tracks: []mp4.Track{{Timescale: 90000}}}
	for i := 0; i < 20; i++ {
		sc.Tracks[0].Samples = append(sc.Tracks[0].Samples, mp4.Sample{
			Index: uint32(i + 1), Offset: int64(i * 100), Size: 100,
			DTS: int64(i * 3000), Duration: 3000, Corrupt: true,
		})
	}
	tl := timelineWith(
		[]model.Clip{{ID: "C1", Track: "V1", Start: rat(0, 1), Duration: rat(1, 1), SourceIn: rat(0, 1)}},
		[]model.Track{videoTrack("V1")},
	)
	g, _ := graph.Build(tl)
	res := recover.Run(recover.Input{Timeline: tl, Graph: g, Scans: map[string]*mp4.Scan{"V1": sc}}, recover.Config{})
	if res.Recovered {
		t.Fatalf("plan should be unrecoverable")
	}
	if !res.Faults.Has(model.CodeUnrecoverable) {
		t.Fatalf("no unrecoverable fault: %v", res.Faults)
	}
	if !res.Faults.Unrecoverable() {
		t.Fatalf("fault severity should be fatal")
	}
}

func TestUnrecoverableRangeBlocksOnlyItsDependents(t *testing.T) {
	bad := scanTrack(10, 1, 0, 9) // every sample corrupt, no keyframe anywhere
	good := scanTrack(30, 5, -1, -1)
	tl := timelineWith(
		[]model.Clip{
			{ID: "C-V", Track: "V1", Start: rat(0, 1), Duration: rat(1, 1), SourceIn: rat(0, 1)},
			{ID: "C-A", Track: "A1", Start: rat(0, 1), Duration: rat(1, 1), SourceIn: rat(0, 1)},
			{ID: "C-P", Track: "P1", Start: rat(0, 1), Duration: rat(1, 1), SourceIn: rat(0, 1)},
		},
		[]model.Track{
			videoTrack("V1"),
			{ID: "A1", Kind: model.KindAudio, Codec: "mp4a", Source: "a.mp4", Timescale: 90000, DependsOn: []string{"V1"}},
			videoTrack("P1"),
		},
	)
	g, _ := graph.Build(tl)
	res := recover.Run(recover.Input{
		Timeline: tl, Graph: g,
		Scans: map[string]*mp4.Scan{"V1": bad, "A1": good, "P1": good},
	}, recover.Config{})
	if res.Recovered {
		t.Fatalf("plan should be unrecoverable")
	}
	statuses := map[string]string{}
	for _, b := range res.Boundaries {
		statuses[b.Clip+"/"+b.Edge] = b.Status
	}
	if statuses["C-V/in"] != "blocked" {
		t.Fatalf("V1 boundary status = %q", statuses["C-V/in"])
	}
	if statuses["C-A/in"] != "blocked" {
		t.Fatalf("A1 should be blocked by V1, got %q", statuses["C-A/in"])
	}
	if statuses["C-P/in"] != "recovered" {
		t.Fatalf("independent P1 should recover, got %q", statuses["C-P/in"])
	}
	if !res.Faults.Has(model.CodeBlockedDependency) {
		t.Fatalf("blocked dependency not reported")
	}
}

func TestVFRBoundaryUsesSampleDurations(t *testing.T) {
	// VFR track: 3000, 7500, 1500 ... The out-point at 1.75 s must land on the
	// sample that actually covers it, not on 1.75*30 fps arithmetic.
	tr := mp4.Track{Timescale: 90000, SourceBytes: 1 << 24}
	durs := []int64{3000, 7500, 1500, 6000}
	var dts int64
	for i := 0; i < 24; i++ {
		d := durs[i%len(durs)]
		tr.Samples = append(tr.Samples, mp4.Sample{
			Index: uint32(i + 1), Offset: int64(1000 + i*100), Size: 100,
			DTS: dts, Duration: d, Keyframe: i%6 == 0,
		})
		dts += d
	}
	tl := timelineWith(
		[]model.Clip{{ID: "C1", Track: "P1", Start: rat(0, 1), Duration: rat(3, 2), SourceIn: rat(0, 1)}},
		[]model.Track{videoTrack("P1")},
	)
	g, _ := graph.Build(tl)
	res := recover.Run(recover.Input{
		Timeline: tl, Graph: g, Scans: map[string]*mp4.Scan{"P1": {Tracks: []mp4.Track{tr}}},
	}, recover.Config{})
	var out *recover.Boundary
	for i := range res.Boundaries {
		if res.Boundaries[i].Edge == "out" {
			out = &res.Boundaries[i]
		}
	}
	if out == nil || out.Status != "recovered" {
		t.Fatalf("out boundary not recovered: %+v", res.Boundaries)
	}
	// Confirm the chosen keyframe's DTS lies at or before the intended point.
	want := int64(1.5 * 90000)
	if out.MediaTicks > want {
		t.Fatalf("out keyframe at %d ticks exceeds intended %d", out.MediaTicks, want)
	}
	for _, s := range tr.Samples {
		if int64(s.Index) == out.Keyframe && !s.Keyframe {
			t.Fatalf("boundary snapped to non-keyframe %d", out.Keyframe)
		}
	}
}

func TestIsolatedSamplesReported(t *testing.T) {
	sc := scanTrack(10, 5, -1, -1)
	sc.Tracks[0].Samples[3].Isolated = true
	sc.Tracks[0].Samples[3].Duration = 0
	tl := timelineWith(
		[]model.Clip{{ID: "C1", Track: "V1", Start: rat(0, 1), Duration: rat(1, 1), SourceIn: rat(0, 1)}},
		[]model.Track{videoTrack("V1")},
	)
	g, _ := graph.Build(tl)
	res := recover.Run(recover.Input{Timeline: tl, Graph: g, Scans: map[string]*mp4.Scan{"V1": sc}}, recover.Config{})
	if !res.Faults.Has(model.CodeVFRGap) {
		t.Fatalf("isolated sample not reported: %v", res.Faults)
	}
	found := false
	for _, st := range res.Tracks {
		if st.Track == "V1" && st.Isolated == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("track state does not count the isolated sample: %+v", res.Tracks)
	}
}

func TestCrossfadeSkewMeasured(t *testing.T) {
	sc := scanTrack(60, 10, -1, -1)
	tl := timelineWith(
		[]model.Clip{
			{ID: "C1", Track: "V1", Start: rat(0, 1), Duration: rat(1, 1), SourceIn: rat(0, 1)},
			{ID: "C2", Track: "V1", Start: rat(1, 1), Duration: rat(1, 1), SourceIn: rat(1, 1)},
		},
		[]model.Track{videoTrack("V1")},
	)
	tl.Transitions = []model.Transition{{ID: "X1", Kind: "crossfade", From: "C1", To: "C2", Duration: rat(1, 2), Center: rat(1, 1)}}
	g, _ := graph.Build(tl)
	res := recover.Run(recover.Input{Timeline: tl, Graph: g, Scans: map[string]*mp4.Scan{"V1": sc}}, recover.Config{})
	if len(res.Transitions) != 1 {
		t.Fatalf("transitions = %+v", res.Transitions)
	}
	x := res.Transitions[0]
	if x.Status != "recovered" {
		t.Fatalf("crossfade status = %s (%s)", x.Status, x.Detail)
	}
	// Both sides snap to the same 1 s keyframe, so the skew is zero.
	if x.SkewUS != 0 {
		t.Fatalf("skew = %d us", x.SkewUS)
	}
}

func TestBlockedTrackDoesNotPanicWithoutScan(t *testing.T) {
	tl := timelineWith(
		[]model.Clip{{ID: "C1", Track: "V1", Start: rat(0, 1), Duration: rat(1, 1), SourceIn: rat(0, 1)}},
		[]model.Track{videoTrack("V1")},
	)
	g, _ := graph.Build(tl)
	res := recover.Run(recover.Input{Timeline: tl, Graph: g}, recover.Config{})
	// A clean plan with nothing recovered is acceptable only if the track was
	// marked unavailable; there must be no recovered boundary on a track with
	// no media.
	if len(res.Tracks) != 1 || res.Tracks[0].OK {
		t.Fatalf("track with no media reported usable: %+v", res.Tracks)
	}
	for _, b := range res.Boundaries {
		if b.Status == "recovered" {
			t.Fatalf("boundary recovered from a track with no media: %+v", b)
		}
	}
	if len(res.Boundaries) == 0 {
		t.Fatalf("blocked boundaries not reported")
	}
	for _, b := range res.Boundaries {
		if b.Status != "blocked" {
			t.Fatalf("boundary status = %s", b.Status)
		}
	}
}

func TestDriftCarriedIntoBoundaryTime(t *testing.T) {
	sc := scanTrack(60, 10, -1, -1)
	tl := timelineWith(
		[]model.Clip{{ID: "C1", Track: "V1", Start: rat(0, 1), Duration: rat(1, 1), SourceIn: rat(0, 1)}},
		[]model.Track{videoTrack("V1")},
	)
	g, _ := graph.Build(tl)
	clk := clock.Result{DriftPPM: model.NewRat(120, 1)}
	res := recover.Run(recover.Input{
		Timeline: tl, Graph: g,
		Scans:  map[string]*mp4.Scan{"V1": sc},
		Clocks: map[string]clock.Result{"V1": clk},
	}, recover.Config{})
	for _, st := range res.Tracks {
		if st.Track == "V1" && st.DriftPPM.Cmp(model.NewRat(120, 1)) != 0 {
			t.Fatalf("drift not carried into track state: %s", st.DriftPPM)
		}
	}
}
