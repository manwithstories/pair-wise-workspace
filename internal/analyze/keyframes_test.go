package analyze_test

import (
	"testing"

	"mediascan/internal/analyze"
	"mediascan/internal/parser"
)

func TestKeyframeIntervalsFromSTSS(t *testing.T) {
	pf := &parser.File{
		Source: &parser.Source{Name: "memory.mp4", SHA256: "test-hash"},
		Tracks: []*parser.Track{{
			TrackID:   7,
			Timescale: 3000,
			Samples: []parser.Sample{
				{Number: 1, DecodeTime: 0, Delta: 1000, Keyframe: true},
				{Number: 2, DecodeTime: 1000, Delta: 1000},
				{Number: 3, DecodeTime: 2000, Delta: 1000},
				{Number: 4, DecodeTime: 3000, Delta: 1000, Keyframe: true},
				{Number: 5, DecodeTime: 4000, Delta: 1000},
				{Number: 6, DecodeTime: 5000, Delta: 1000, Keyframe: true},
			},
		}},
	}

	reports := analyze.AnalyzeKeyframes(pf, 9)
	if len(reports) != 1 {
		t.Fatalf("reports = %d, want 1", len(reports))
	}
	r := reports[0]
	if r.FileIndex != 9 || r.TrackID != 7 {
		t.Fatalf("identity = file %d track %d", r.FileIndex, r.TrackID)
	}
	if got := len(r.Keyframes); got != 3 {
		t.Fatalf("keyframes = %d, want 3", got)
	}
	wantTicks := []int64{0, 3000, 5000}
	for i, k := range r.Keyframes {
		if k.Ticks != wantTicks[i] {
			t.Fatalf("keyframe %d ticks = %d, want %d", i, k.Ticks, wantTicks[i])
		}
	}
	if got := len(r.Intervals); got != 2 {
		t.Fatalf("intervals = %d, want 2", got)
	}
	if r.Intervals[0].Ticks != 3000 || r.Intervals[1].Ticks != 2000 {
		t.Fatalf("interval ticks = %d,%d", r.Intervals[0].Ticks, r.Intervals[1].Ticks)
	}
	if max := analyze.MaxKeyframeTickError(r.Keyframes, wantTicks); max != 0 {
		t.Fatalf("max tick error = %d, want 0", max)
	}
}

func TestFragmentKeyframeTimes(t *testing.T) {
	pf := &parser.File{
		Source: &parser.Source{Name: "frag.mp4"},
		Tracks: []*parser.Track{{
			TrackID:   1,
			Timescale: 30000,
			Fragments: []parser.Fragment{{
				BaseTime: 9000,
				Samples: []parser.FragmentSample{
					{Number: 1, Duration: 1001, Keyframe: true},
					{Number: 2, Duration: 1001, Keyframe: false},
					{Number: 3, Duration: 1001, Keyframe: true},
				},
			}},
		}},
	}
	r := analyze.AnalyzeKeyframes(pf, 0)[0]
	want := []int64{9000, 11002}
	if len(r.Keyframes) != 2 {
		t.Fatalf("keyframes = %d", len(r.Keyframes))
	}
	for i, wantTick := range want {
		if got := r.Keyframes[i].Ticks; got != wantTick {
			t.Fatalf("keyframe %d = %d, want %d (error %d ticks)", i, got, wantTick, got-wantTick)
		}
	}
	if r.Intervals[0].Ticks != 2002 {
		t.Fatalf("fragment interval = %d, want 2002", r.Intervals[0].Ticks)
	}
}
