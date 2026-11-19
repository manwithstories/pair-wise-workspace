package segment_test

import (
	"testing"

	"mediasync/internal/media"
	"mediasync/internal/mp4"
	"mediasync/internal/segment"
)

func syntheticTrack(samples int, keyEvery int) *mp4.Track {
	tr := &mp4.Track{File: "synthetic.mp4", TrackID: 1, Handler: "vide", DurationNS: int64(samples) * 100_000_000}
	for i := 0; i < samples; i++ {
		tr.Samples = append(tr.Samples, mp4.Sample{
			Number: uint32(i + 1), Offset: int64(i * 10), Size: 10,
			DTSNS: int64(i) * 100_000_000, DurationNS: 100_000_000,
		})
	}
	for i := 0; i < samples; i += keyEvery {
		tr.Keyframes = append(tr.Keyframes, i)
		tr.Samples[i].Keyframe = true
	}
	return tr
}

func TestCutSnapsToKeyframes(t *testing.T) {
	tr := syntheticTrack(80, 20) // keyframes every 2s
	res := segment.Cut(tr, segment.Request{Name: "cam", TargetMS: 1500, OverlapMS: 200, MinMS: 400})
	if len(res.Segments) == 0 {
		t.Fatalf("no segments: %v", res.Issues)
	}
	for _, s := range res.Segments {
		flagged := false
		for _, d := range s.Degrade {
			if d == segment.DegradeNoKeyframe {
				flagged = true
			}
		}
		if !tr.Samples[s.StartSample].Keyframe && !flagged {
			t.Errorf("segment %d starts on sample %d, which is neither a keyframe nor flagged", s.Index, s.StartSample)
		}
		if s.EndNS <= s.StartNS {
			t.Errorf("segment %d has non-positive duration", s.Index)
		}
		if s.SourceFile != tr.File {
			t.Errorf("segment %d source = %q", s.Index, s.SourceFile)
		}
	}
	if len(res.Segments) > 1 && res.Segments[1].StartNS == res.Segments[0].StartNS {
		t.Error("consecutive segments start at the same instant")
	}
}

func TestCutFlagsGapBoundaries(t *testing.T) {
	tr := syntheticTrack(80, 20)
	// A gap that overlaps the end of the first clip (1.5s) and the start of
	// the second (2.0s).
	gap := segment.Gap{StartNS: 1_400_000_000, EndNS: 2_100_000_000}
	res := segment.Cut(tr, segment.Request{Name: "cam", TargetMS: 1500, OverlapMS: 0, MinMS: 400,
		Gaps: []segment.Gap{gap}})
	var degraded []segment.DegradeReason
	for _, s := range res.Segments {
		degraded = append(degraded, s.Degrade...)
	}
	found := false
	for _, d := range degraded {
		if d == segment.DegradeGap {
			found = true
		}
	}
	if !found {
		t.Fatalf("gap overlap not flagged: %+v", res.Segments)
	}
	hasOffsetIssue := false
	for _, is := range res.Issues {
		if is.Code == media.CodeOffsetInsideGap && is.HasOffset {
			hasOffsetIssue = true
		}
	}
	if !hasOffsetIssue {
		t.Errorf("no offset-bearing gap issue: %v", res.Issues)
	}
}

func TestCutWithoutKeyframesDegrades(t *testing.T) {
	tr := syntheticTrack(50, 50)
	tr.Keyframes = []int{0}
	res := segment.Cut(tr, segment.Request{Name: "cam", TargetMS: 1000, MinMS: 100})
	if len(res.Segments) == 0 {
		t.Fatalf("no segments: %v", res.Issues)
	}
	flagged := false
	for _, s := range res.Segments[1:] {
		for _, d := range s.Degrade {
			if d == segment.DegradeNoKeyframe {
				flagged = true
			}
		}
	}
	if !flagged {
		t.Errorf("clips without keyframes were not degraded: %+v", res.Segments)
	}
}

func TestCutTailPullsBackToKeyframe(t *testing.T) {
	tr := syntheticTrack(80, 20)
	res := segment.Cut(tr, segment.Request{Name: "cam", TargetMS: 1500, OverlapMS: 200, MinMS: 100})
	n := len(res.Segments)
	if n == 0 {
		t.Fatal("no segments")
	}
	last := res.Segments[n-1]
	flagged := false
	for _, d := range last.Degrade {
		if d == segment.DegradeNoKeyframe {
			flagged = true
		}
	}
	if !tr.Samples[last.StartSample].Keyframe && !flagged {
		t.Errorf("tail clip starts at sample %d without a keyframe or degrade flag", last.StartSample)
	}
	if last.EndNS != tr.Samples[len(tr.Samples)-1].EndNS() {
		t.Errorf("tail clip ends at %d, track ends at %d", last.EndNS, tr.Samples[len(tr.Samples)-1].EndNS())
	}
}

func TestCutRejectsBadRequests(t *testing.T) {
	tr := syntheticTrack(10, 5)
	cases := []struct {
		name string
		req  segment.Request
	}{
		{"zero target", segment.Request{Name: "cam", TargetMS: 0}},
		{"overlap too large", segment.Request{Name: "cam", TargetMS: 1000, OverlapMS: 1000}},
		{"range past end", segment.Request{Name: "cam", TargetMS: 1000, StartNS: 99_000_000_000}},
	}
	for _, c := range cases {
		res := segment.Cut(tr, c.req)
		if len(res.Segments) != 0 {
			t.Errorf("%s: produced %d segments", c.name, len(res.Segments))
		}
		if len(res.Issues) == 0 {
			t.Errorf("%s: no issue reported", c.name)
		}
	}
	empty := segment.Cut(&mp4.Track{File: "empty.mp4"}, segment.Request{Name: "cam", TargetMS: 1000})
	if len(empty.Issues) == 0 {
		t.Error("empty track produced no issue")
	}
}

func TestCutOverlapProducesCoverage(t *testing.T) {
	// Keyframes every 100ms so the seek-back into the overlap window can be
	// honoured exactly.
	tr := syntheticTrack(100, 1)
	res := segment.Cut(tr, segment.Request{Name: "cam", TargetMS: 2000, OverlapMS: 500, MinMS: 100})
	if len(res.Segments) < 2 {
		t.Fatalf("got %d segments", len(res.Segments))
	}
	for i := 1; i < len(res.Segments); i++ {
		prev, cur := res.Segments[i-1], res.Segments[i]
		if cur.StartNS >= prev.EndNS {
			t.Errorf("segment %d starts at %d after previous end %d: overlap lost", i, cur.StartNS, prev.EndNS)
		}
		if cur.StartNS <= prev.StartNS {
			t.Errorf("segment %d does not advance: %d <= %d", i, cur.StartNS, prev.StartNS)
		}
	}
}

func TestSegmentCountLimit(t *testing.T) {
	tr := syntheticTrack(segment.MaxSegments+10, 1)
	res := segment.Cut(tr, segment.Request{Name: "cam", TargetMS: 100, MinMS: 1})
	if len(res.Segments) > segment.MaxSegments {
		t.Errorf("produced %d segments, limit is %d", len(res.Segments), segment.MaxSegments)
	}
	limited := false
	for _, is := range res.Issues {
		if is.Code == media.CodeLimitExceeded {
			limited = true
		}
	}
	if !limited {
		t.Error("limit exceeded not reported")
	}
}
