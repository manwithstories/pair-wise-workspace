package manifest

import (
	"testing"

	"mediameld/internal/mapping"
	"mediameld/internal/model"
	"mediameld/internal/remux"
	"mediameld/internal/timeline"
)

func mkPlan() plan2 {
	// Build a tiny stable plan: two tracks, one gap, one late replay.
	events := []model.Event{
		{TrackID: "1", Fingerprint: "fp1", Kind: model.KindVideo, Index: 0, Seq: 1, PTS: 0, Duration: 1_000_000, Hash: hb(1), Size: 4},
		{TrackID: "1", Fingerprint: "fp1", Kind: model.KindVideo, Index: 1, Seq: 3, PTS: 40_000_000, Duration: 1_000_000, Hash: hb(2), Size: 4},
		{TrackID: "2", Fingerprint: "fp2", Kind: model.KindAudio, Index: 0, Seq: 1, PTS: 0, Duration: 33_000_000, Hash: hb(3), Size: 6},
	}
	plan, err := timeline.Build(events, timeline.Config{AllowedLate: 20_000_000, SegmentSpan: 1_000_000_000})
	if err != nil {
		panic(err)
	}
	return plan2{plan}
}

type plan2 struct{ timeline.Plan }

func hb(b byte) [32]byte {
	var x [32]byte
	x[0] = b
	return x
}

func TestManifestRecordsHashesGapsAndMetadata(t *testing.T) {
	p := mkPlan()
	video := &model.Track{ID: "1", Kind: model.KindVideo, Timescale: 90000, CodedWidth: 640, CodedHeight: 360}
	video.ContainerColor = model.ColorDesc{Primaries: "bt709", Transfer: "bt709", Matrix: "bt709", FullRange: "limited"}
	video.ContainerRotation = 90
	resolved := []mapping.Resolution{{
		Role: mapping.RoleMainVideo, Fingerprint: "fp1", Track: video,
		Decision: mapping.DisplayDecision{
			Rotation: 90,
			Color:    video.ContainerColor,
			Reasons: []mapping.Reason{
				{Field: "rotation", Source: "container", Original: "90", Chosen: "90", Why: "tkhd"},
			},
		},
	}}
	remuxRes := remux.Result{
		SampleHashes: []remux.SampleHash{
			{TrackID: "1", Index: 0, Hash: hb(1), Size: 4},
			{TrackID: "1", Index: 1, Hash: hb(2), Size: 4},
			{TrackID: "2", Index: 0, Hash: hb(3), Size: 6},
		},
	}
	m := Build(p.Plan, resolved, remuxRes, []SourceRecord{{Path: "a.mp4", Fingerprint: "src", Bytes: 10}})
	if m.SamplesCount != 3 || len(m.Samples) != 3 {
		t.Fatalf("samples=%d/%d", m.SamplesCount, len(m.Samples))
	}
	if len(m.Gaps) != 1 {
		t.Fatalf("gaps=%d want 1", len(m.Gaps))
	}
	if m.TimelineHash != p.Hash {
		t.Fatalf("timeline hash not recorded")
	}
	if len(m.Metadata) == 0 {
		t.Fatal("metadata decisions not recorded")
	}
	if m.Quality.Matched != 3 || m.Quality.MatchRatePct != 100 {
		t.Fatalf("quality=%+v", m.Quality)
	}
}

func TestVerifierDetectsHashMismatch(t *testing.T) {
	p := mkPlan()
	m := Build(p.Plan, nil, remux.Result{SampleHashes: []remux.SampleHash{
		{TrackID: "1", Index: 0, Hash: hb(1)}, {TrackID: "1", Index: 1, Hash: hb(2)},
	}}, nil)
	// The output claims a different hash for the second sample.
	output := []model.Event{
		{TrackID: "1", Index: 0, PTS: 0, Duration: 1_000_000, Hash: hb(1)},
		{TrackID: "1", Index: 1, PTS: 40_000_000, Duration: 1_000_000, Hash: hb(99)},
	}
	v := Verifier{Demux: func() ([]model.Event, error) { return output, nil }}
	rep, err := v.Verify(m, 1_000_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if rep.HashMismatches != 1 {
		t.Fatalf("mismatches=%d want 1", rep.HashMismatches)
	}
	if rep.OK() {
		t.Fatal("verifier passed a known-bad output")
	}
}

func TestVerifierAlignmentWithinOneMS(t *testing.T) {
	p := mkPlan()
	m := Build(p.Plan, nil, remux.Result{SampleHashes: []remux.SampleHash{
		{TrackID: "1", Index: 0, Hash: hb(1)}, {TrackID: "1", Index: 1, Hash: hb(2)},
	}}, nil)
	// Output start is off by 0.4ms, inside the 1ms tolerance.
	output := []model.Event{
		{TrackID: "1", Index: 0, PTS: model.Instant(400_000), Duration: 1_000_000, Hash: hb(1)},
		{TrackID: "1", Index: 1, PTS: model.Instant(40_400_000), Duration: 1_000_000, Hash: hb(2)},
	}
	v := Verifier{Demux: func() ([]model.Event, error) { return output, nil }}
	rep, _ := v.Verify(m, 1_000_000_000)
	if rep.AlignmentMaxMS > 1 {
		t.Fatalf("alignment=%dms exceeds 1ms", rep.AlignmentMaxMS)
	}
}

func TestManifestRoundTrip(t *testing.T) {
	p := mkPlan()
	m := Build(p.Plan, nil, remux.Result{SampleHashes: []remux.SampleHash{{TrackID: "1", Index: 0, Hash: hb(1)}}}, nil)
	path := t.TempDir() + "/manifest.json"
	if err := Write(path, m); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.TimelineHash != m.TimelineHash || len(got.Samples) != len(m.Samples) {
		t.Fatal("round trip lost data")
	}
}
