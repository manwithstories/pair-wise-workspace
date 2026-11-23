package segments_test

import (
	"bytes"
	"testing"

	"bcast/demux"
	"bcast/internal/fixture"
	"bcast/media"
	"bcast/segments"
)

func cutFixture(t *testing.T, spec fixture.Spec) []segments.Segment {
	t.Helper()
	a, err := fixture.Generate(spec)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	f, err := demux.Parse(bytes.NewReader(a.Clean), int64(len(a.Clean)), "clean")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	segs, err := segments.Cut(&f.File, 90000) // 1s @90kHz
	if err != nil {
		t.Fatalf("cut: %v", err)
	}
	return segs
}

func TestCutTwelveSegments(t *testing.T) {
	segs := cutFixture(t, fixture.Default(17, 12))
	if len(segs) != 12 {
		t.Fatalf("segments=%d", len(segs))
	}
	for i, s := range segs {
		if len(s.Video) != 30 {
			t.Fatalf("seg %d video=%d", i, len(s.Video))
		}
		if len(s.Audio) != 10 {
			t.Fatalf("seg %d audio=%d", i, len(s.Audio))
		}
		if s.End.Value-s.Start.Value != 90000 {
			t.Fatalf("seg %d duration=%d", i, s.End.Value-s.Start.Value)
		}
	}
}

func TestKeyframeDeviation(t *testing.T) {
	segs := cutFixture(t, fixture.Default(17, 12))
	hit, total := segments.KeyframeHitRate(segs)
	if total != 11 || hit != 10 {
		t.Fatalf("hit=%d/%d", hit, total)
	}
	// Segment 5 opens on a non-keyframe frame: the distance to the nearest
	// keyframe is recorded while the cut stays on a real frame start.
	b := segs[5].Boundary
	if b.OnKeyframe {
		t.Fatal("seg 5 boundary should miss keyframe")
	}
	if b.KeyframeDist == 0 {
		t.Fatal("keyframe distance not recorded")
	}
	for i, s := range segs {
		if s.Boundary.Ideal.Value == 0 {
			continue
		}
		if s.Boundary.Deviation != s.Boundary.Actual.Value-s.Boundary.Ideal.Value {
			t.Fatalf("seg %d deviation inconsistent", i)
		}
	}
}

func TestSnapHandlesVFRFrames(t *testing.T) {
	// Two frames per target second, with a long gap: the cut must land on a
	// real frame start, never on an interpolated time.
	f := &media.File{
		Tracks: []media.Track{{ID: 1, Kind: media.TrackVideo, Timescale: 1000}},
		Samples: []media.Sample{
			{TrackID: 1, Kind: media.TrackVideo, DTS: media.Time{Value: 0, Timescale: 1000}, Duration: 400, Keyframe: true},
			{TrackID: 1, Kind: media.TrackVideo, DTS: media.Time{Value: 400, Timescale: 1000}, Duration: 900},
			{TrackID: 1, Kind: media.TrackVideo, DTS: media.Time{Value: 1300, Timescale: 1000}, Duration: 400, Keyframe: true},
			{TrackID: 1, Kind: media.TrackVideo, DTS: media.Time{Value: 1700, Timescale: 1000}, Duration: 300},
		},
	}
	segs, err := segments.Cut(f, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 2 {
		t.Fatalf("segments=%d", len(segs))
	}
	if got := segs[1].Start.Value; got != 1300 {
		t.Fatalf("snapped start=%d want 1300", got)
	}
	if got := segs[1].Boundary.Deviation; got != 300 {
		t.Fatalf("deviation=%d", got)
	}
}

func TestTargetShorterThanFrameDoesNotEmitEmpty(t *testing.T) {
	f := &media.File{
		Tracks: []media.Track{{ID: 1, Kind: media.TrackVideo, Timescale: 1000}},
		Samples: []media.Sample{
			{TrackID: 1, Kind: media.TrackVideo, DTS: media.Time{Value: 0, Timescale: 1000}, Duration: 500, Keyframe: true},
			{TrackID: 1, Kind: media.TrackVideo, DTS: media.Time{Value: 500, Timescale: 1000}, Duration: 500},
		},
	}
	segs, err := segments.Cut(f, 200)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int64]bool{}
	for _, s := range segs {
		if s.Start.Value >= s.End.Value {
			t.Fatalf("empty segment %d [%d,%d)", s.Index, s.Start.Value, s.End.Value)
		}
		if seen[s.Start.Value] {
			t.Fatalf("duplicate boundary at %d", s.Start.Value)
		}
		seen[s.Start.Value] = true
	}
}

func TestAudioAlignment(t *testing.T) {
	segs := cutFixture(t, fixture.Default(17, 12))
	for i, s := range segs {
		if len(s.Audio) == 0 {
			continue
		}
		wantStart := s.Start.ToTimescale(48000).Value
		if s.Audio[0].DTS.Value != wantStart {
			t.Fatalf("seg %d audio start=%d want %d", i, s.Audio[0].DTS.Value, wantStart)
		}
	}
}
