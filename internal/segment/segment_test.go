package segment

import (
	"testing"

	"mediacheck/internal/clock"
	"mediacheck/internal/fixture"
	"mediacheck/internal/index"
	"mediacheck/internal/probe"
)

func mustEncode(m *Manifest) []byte {
	_, data, err := index.DigestJSON(m)
	if err != nil {
		panic(err)
	}
	return data
}

func parseVideoTrack(t *testing.T, o fixture.Options) *probe.Track {
	t.Helper()
	m, _, err := fixture.GenerateFile(o)
	if err != nil {
		t.Fatal(err)
	}
	res, err := probe.Parse(m.ReaderAt(), int64(m.Len()), "rep")
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range res.Tracks {
		if tr.IsVideo {
			return tr
		}
	}
	t.Fatal("no video track")
	return nil
}

func repOpts(seed int64, keyEvery uint32) fixture.Options {
	return fixture.Options{
		Seed: seed, DurationSeconds: 12, VideoTimescale: 90000, VideoFPS: 25,
		AudioTimescale: 48000, AudioDelta: 9600, KeyEvery: keyEvery,
		StartDTS: 90000, EditList: true, VideoBody: 12, AudioBody: 6,
		VideoStreamID: fixture.StreamVideo(), AudioStreamID: fixture.StreamAudio(),
	}
}

func TestSharedCutGridAcrossRepresentations(t *testing.T) {
	p, err := NewPlanner(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	reps := []string{"360p", "720p", "1080p"}
	var plans []TrackPlan
	var anchor TrackPlan
	for i, name := range reps {
		tr := parseVideoTrack(t, repOpts(int64(100+i), 25))
		tp := p.PlanTrack(name, tr)
		plans = append(plans, tp)
		if i == 0 {
			anchor = tp
		}
	}
	merged, alignErrs := p.Align(anchor, plans[1:])
	if len(merged.Representations) != 3 {
		t.Fatalf("representations = %d, want 3", len(merged.Representations))
	}
	// 12 s timeline with 6 s target gives cuts at 0 and 6 s.
	if len(merged.Cuts) != 2 {
		t.Fatalf("cuts = %d, want 2", len(merged.Cuts))
	}
	if err := ValidateAlignment(merged, clock.MillisecondsToTicks(40)); err != nil {
		t.Fatalf("alignment invalid: %v (errors=%d)", err, len(alignErrs))
	}
}

func TestBoundedFallbackOnDrift(t *testing.T) {
	cfg := PlanConfig{
		TargetTicks: clock.DurationTicks(6), WindowTicks: clock.DurationTicks(3),
		MaxDriftTicks: clock.MillisecondsToTicks(1),
	}
	p, _ := NewPlanner(cfg)
	anchor := p.PlanTrack("1080p", parseVideoTrack(t, repOpts(1, 25)))
	// Keyframes every 4 s cannot meet the 6 s grid cut, forcing a real
	// boundary drift that the bounded fallback must record.
	other := p.PlanTrack("360p", parseVideoTrack(t, repOpts(2, 100)))
	_, errs := p.Align(anchor, []TrackPlan{other})
	if len(errs) == 0 {
		t.Fatal("sparse-keyframe representation must record at least one drift error")
	}
	for _, e := range errs {
		if e.DriftTicks <= cfg.MaxDriftTicks {
			t.Fatal("only over-limit drift should be reported")
		}
	}
}

func TestSparseKeyframeProducesExplicitGap(t *testing.T) {
	cfg := PlanConfig{
		TargetTicks: clock.DurationTicks(6), WindowTicks: clock.MillisecondsToTicks(100),
		MaxDriftTicks: clock.MillisecondsToTicks(40),
	}
	p, _ := NewPlanner(cfg)
	// Keyframes every 8 s (200 frames at 25 fps): the 6 s grid cut has no
	// keyframe inside the 100 ms window and must become an explicit gap.
	tr := parseVideoTrack(t, repOpts(7, 200))
	tp := p.PlanTrack("360p", tr)
	gapSeen := false
	for _, c := range tp.Cuts {
		if c.HasGap && (c.Gap.End <= c.Gap.Start) {
			t.Fatal("gap must carry a positive span")
		}
		if c.HasGap {
			gapSeen = true
		}
	}
	if !gapSeen {
		t.Fatal("expected an explicit no-decodable-boundary gap")
	}
}

func TestAudioBoundaryFallbackAndSync(t *testing.T) {
	m, _, err := fixture.GenerateFile(repOpts(5, 25))
	if err != nil {
		t.Fatal(err)
	}
	res, err := probe.Parse(m.ReaderAt(), int64(m.Len()), "rep")
	if err != nil {
		t.Fatal(err)
	}
	var audio *probe.Track
	for _, tr := range res.Tracks {
		if !tr.IsVideo {
			audio = tr
		}
	}
	// Request a cut deliberately offset from any audio sample boundary.
	ideal := audio.Samples[1].StartTicks + clock.MillisecondsToTicks(10)
	cuts := []Cut{{Index: 0, IdealAt: 0, ChosenAt: 0}, {Index: 1, IdealAt: ideal, ChosenAt: ideal}}
	out, aligns := AlignAudioTrack(audio, cuts, clock.MillisecondsToTicks(40))
	if len(aligns) == 0 {
		t.Fatal("audio alignment produced no measurements")
	}
	if MaxAVSyncMillis(aligns) > 40 {
		t.Fatalf("audio residual exceeds bounded fallback: %d ms", MaxAVSyncMillis(aligns))
	}
	if out[1].ChosenAt != audio.Samples[1].StartTicks {
		t.Fatalf("audio cut must snap to nearest sample boundary %d, got %d",
			audio.Samples[1].StartTicks, out[1].ChosenAt)
	}
}

func TestManifestByteDeterminism(t *testing.T) {
	p, _ := NewPlanner(DefaultConfig())
	tp := p.PlanTrack("720p", parseVideoTrack(t, repOpts(9, 25)))
	merged, _ := p.Align(tp, nil)
	segs := BuildSegments(merged, nil)
	m1 := FinalizeManifest("digest-abc", merged, segs, nil, nil, nil)
	m2 := FinalizeManifest("digest-abc", merged, segs, nil, nil, nil)
	b1 := mustEncode(m1)
	b2 := mustEncode(m2)
	if string(b1) != string(b2) {
		t.Fatal("identical inputs must encode to identical bytes")
	}
}
