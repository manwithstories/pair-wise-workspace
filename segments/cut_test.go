package segments_test

import (
	"bytes"
	"testing"

	"bcast/demux"
	"bcast/internal/fixture"
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
	// Segment 5 opens without a keyframe: deviation to nearest keyframe.
	b := segs[5].Boundary
	if b.OnKeyframe {
		t.Fatal("seg 5 boundary should miss keyframe")
	}
	if b.Deviation == 0 {
		t.Fatal("deviation not recorded")
	}
	if b.Actual.Value != b.Ideal.Value {
		t.Fatal("cut must stay frame-exact")
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
