package timeline

import (
	"math"
	"testing"

	"pairflow/internal/media"
)

func mustResolve(t *testing.T, tl *RawTimeline) []Resolved {
	t.Helper()
	res, err := Resolve(tl)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return res
}

func TestGenerateResolvesCleanly(t *testing.T) {
	tl, err := Generate(17, 3)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	res := mustResolve(t, tl)
	if len(res) != 4 {
		t.Fatalf("want 4 tracks, got %d", len(res))
	}
	for _, tr := range res {
		for i := 1; i < len(tr.Segments); i++ {
			if tr.Segments[i].Start.Cmp(tr.Segments[i-1].End) != 0 {
				t.Fatalf("track %s not continuous at segment %d: %s then %s",
					tr.TrackID, i, tr.Segments[i-1].End, tr.Segments[i].Start)
			}
		}
		segs := tr.Segments
		for i := 0; i+1 < len(segs); i++ {
			if !segs[i].DissolveOut.Equal(segs[i+1].DissolveIn) {
				t.Fatalf("track %s dissolve window disagrees: out %s in %s",
					tr.TrackID, segs[i].DissolveOut, segs[i+1].DissolveIn)
			}
		}
	}
}

func TestGenerateFrameAndSampleCounts(t *testing.T) {
	tl, _ := Generate(17, 3)
	frames, err := tl.TotalFrames()
	if err != nil || frames != 72 {
		t.Fatalf("3s @24fps = 72 frames, got %d (%v)", frames, err)
	}
	samples, err := tl.TotalSamples()
	if err != nil || samples != 144000 {
		t.Fatalf("3s @48kHz = 144000 samples, got %d (%v)", samples, err)
	}
}

func TestFingerprintStableAndSeedSensitive(t *testing.T) {
	a1, _ := Generate(17, 3)
	a2, _ := Generate(17, 3)
	ra := mustResolve(t, a1)
	rb := mustResolve(t, a2)
	if Fingerprint(ra) == "" {
		t.Fatal("fingerprint empty")
	}
	if Fingerprint(ra) != Fingerprint(rb) {
		t.Fatal("same seed must produce identical timeline fingerprint")
	}
	if Fingerprint(mustResolve(t, mustGenerate(t, 18, 3))) == Fingerprint(ra) {
		t.Fatal("different seed must change fingerprint")
	}
}

func TestDecodeRejectsUnknownFieldsAndKinds(t *testing.T) {
	// Decode rejects structural surprises; transition semantics are checked by
	// Resolve (see TestResolveRejectsBadEdits).
	cases := map[string]string{
		"unknown field":      `{"version":1,"fps":24,"sample_rate":48000,"duration":{"num":3,"den":1},"tracks":[],"bogus":1}`,
		"unknown track kind": `{"version":1,"fps":24,"sample_rate":48000,"duration":{"num":3,"den":1},"tracks":[{"id":"t","kind":"midi","clips":[{"id":"c","start":{"num":0,"den":1},"end":{"num":1,"den":1}}]}]}`,
		"bad rate":           `{"version":1,"fps":24,"sample_rate":44100,"duration":{"num":3,"den":1},"tracks":[]}`,
		"missing clip spec":  `{"version":1,"fps":24,"sample_rate":48000,"duration":{"num":3,"den":1},"tracks":[{"id":"v","kind":"video","clips":[{"id":"c","start":{"num":0,"den":1},"end":{"num":1,"den":1}}]}]}`,
	}
	for name, doc := range cases {
		if _, err := DecodeBytes([]byte(doc), name); err == nil {
			t.Fatalf("%s: unknown construct was silently accepted", name)
		} else if err.Class != media.ClassInput {
			t.Fatalf("%s: want class-2, got %s", name, err.Class)
		}
	}
}

func TestResolveRejectsUnknownTransitionKind(t *testing.T) {
	tl := mustGenerate(t, 17, 3)
	tl.Tracks[0].Transitions[0].Kind = "wipe"
	if _, err := Resolve(tl); err == nil || err.Code != "timeline.unknown_transition" {
		t.Fatalf("unknown transition kind must be rejected, got %v", err)
	}
}

func TestResolveRejectsBadEdits(t *testing.T) {
	base := func(tr Track) *RawTimeline {
		return &RawTimeline{Version: 1, FPS: 24, SampleRate: 48000,
			Duration: RawTime{Num: 4, Den: 1}, Tracks: []Track{tr}}
	}
	vid := func() *VideoSpec { return &VideoSpec{Space: "srgb", Pattern: "bars", Tint: "teal"} }
	cases := []struct {
		name string
		tl   *RawTimeline
		code string
	}{
		{"reversed cut", base(Track{ID: "v", Kind: "video", Clips: []Clip{
			{ID: "c1", Start: RawTime{1, 1}, End: RawTime{1, 2}, Video: vid()}}}),
			"timeline.reversed_cut"},
		{"clip beyond duration", base(Track{ID: "v", Kind: "video", Clips: []Clip{
			{ID: "c1", Start: RawTime{0, 1}, End: RawTime{9, 2}, Video: vid()}}}),
			"timeline.out_of_bounds"},
		{"dissolve longer than clip", base(Track{ID: "v", Kind: "video", Clips: []Clip{
			{ID: "c1", Start: RawTime{0, 1}, End: RawTime{1, 2}, Video: vid()},
			{ID: "c2", Start: RawTime{1, 2}, End: RawTime{2, 1}, Video: vid()}},
			Transitions: []Transition{{Kind: "cross_dissolve", FromClip: "c1", ToClip: "c2", Duration: RawTime{3, 4}}}}),
			"timeline.transition_oob"},
		{"overlap without transition", base(Track{ID: "v", Kind: "video", Clips: []Clip{
			{ID: "c1", Start: RawTime{0, 1}, End: RawTime{1, 1}, Video: vid()},
			{ID: "c2", Start: RawTime{1, 2}, End: RawTime{2, 1}, Video: vid()}}}),
			"timeline.clip_overlap"},
		{"transition over gap", base(Track{ID: "v", Kind: "video", Clips: []Clip{
			{ID: "c1", Start: RawTime{0, 1}, End: RawTime{1, 1}, Video: vid()},
			{ID: "c2", Start: RawTime{6, 5}, End: RawTime{2, 1}, Video: vid()}},
			Transitions: []Transition{{Kind: "cross_dissolve", FromClip: "c1", ToClip: "c2", Duration: RawTime{1, 10}}}}),
			"timeline.transition_gap"},
	}
	for _, c := range cases {
		_, err := Resolve(c.tl)
		if err == nil {
			t.Fatalf("%s: accepted", c.name)
		}
		if err.Code != c.code {
			t.Fatalf("%s: want code %s, got %s", c.name, c.code, err.Code)
		}
		if err.Class != media.ClassInput {
			t.Fatalf("%s: want class-2, got %s", c.name, err.Class)
		}
	}
}

func TestResolveOverflowRejected(t *testing.T) {
	tl := &RawTimeline{Version: 1, FPS: 24, SampleRate: 48000,
		Duration: RawTime{Num: 1, Den: 1},
		Tracks: []Track{{ID: "v", Kind: "video", Clips: []Clip{
			{ID: "c1", Start: RawTime{Num: math.MaxInt64, Den: 1}, End: RawTime{Num: 0, Den: 1}, Video: &VideoSpec{Space: "srgb"}},
		}}}}
	if _, err := Resolve(tl); err == nil || err.Class != media.ClassInput {
		t.Fatalf("overflowing boundary must be class-2, got %v", err)
	}
}

func mustGenerate(t *testing.T, seed uint64, sec int) *RawTimeline {
	tl, err := Generate(seed, sec)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return tl
}
