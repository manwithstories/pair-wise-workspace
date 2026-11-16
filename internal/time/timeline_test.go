package timeline

import (
	"testing"

	"mp4loc/internal/box"
)

func trackWithSamples(dts ...int64) *box.Track {
	tr := &box.Track{ID: 1, Handler: "vide", Timescale: 1000, MdhdVersion: 0}
	tr.Table = &box.SampleTable{}
	for i, d := range dts {
		tr.Table.Samples = append(tr.Table.Samples, box.SampleRecord{Index: i, DTS: d, PTS: d})
	}
	return tr
}

func TestMapToMediaReasons(t *testing.T) {
	tr := trackWithSamples(0, 1000, 2000)
	tr.Edits = []box.EditSegment{
		{SegmentDuration: 500, MediaTime: -1, MediaRate: 1},  // gap
		{SegmentDuration: 2000, MediaTime: 0, MediaRate: 1}, // media
	}
	tl := Build(1000, tr)

	if r := tl.MapToMedia(-1); r.OK || r.Reason != "before_first_segment" {
		t.Fatalf("negative target: %+v", r)
	}
	if r := tl.MapToMedia(250); r.OK || r.Reason != "empty_edit" || r.Boundary != 500 {
		t.Fatalf("gap target: %+v", r)
	}
	if r := tl.MapToMedia(2500); r.OK || r.Reason != "beyond_duration" || r.Boundary != 2500 {
		t.Fatalf("late target: %+v", r)
	}
	r := tl.MapToMedia(1500) // 1s into the media segment
	if !r.OK || r.MediaTime != 1000 {
		t.Fatalf("mid target: %+v", r)
	}
}

func TestWrapDetection(t *testing.T) {
	// Media origin just below 2^32: the third sample crosses it.
	origin := (int64(1) << 32) - 1500
	tr := trackWithSamples(0, 1000, 2000, 3000)
	tr.Edits = []box.EditSegment{{SegmentDuration: 4000, MediaTime: origin, MediaRate: 1}}
	tl := Build(1000, tr)

	if len(tl.WrapEvents) != 1 {
		t.Fatalf("WrapEvents = %d, want 1", len(tl.WrapEvents))
	}
	we := tl.WrapEvents[0]
	if we.Sample != 2 || we.Raw != 500 || we.Unwrapped != origin+2000 {
		t.Fatalf("bad wrap event: %+v", we)
	}
	// Raw wraps to a small value; normalized stays monotonic.
	raw, norm := tl.Normalize(2000)
	if raw != 500 || norm != 2000 {
		t.Fatalf("Normalize(2000) = (%d,%d), want (500,2000)", raw, norm)
	}
}

func TestNegativeOrigin(t *testing.T) {
	tr := trackWithSamples(0, 1000)
	tr.Edits = []box.EditSegment{{SegmentDuration: 1000, MediaTime: -500, MediaRate: 1}}
	tl := Build(1000, tr)
	if tl.MediaOrigin != -500 {
		t.Fatalf("MediaOrigin = %d, want -500", tl.MediaOrigin)
	}
	raw, norm := tl.Normalize(0)
	// 32-bit domain: -500 projects to its uint32 wraparound.
	if raw != (1<<32)-500 || norm != 0 {
		t.Fatalf("Normalize(0) = (%d,%d), want (%d,0)", raw, norm, (1<<32)-500)
	}
}

func TestNoEditsIdentity(t *testing.T) {
	tr := trackWithSamples(0, 1000, 2000)
	tr.Duration = 3000
	tl := Build(1000, tr)
	if len(tl.Segments) != 1 || tl.Duration != 3000 {
		t.Fatalf("segments: %+v dur=%d", tl.Segments, tl.Duration)
	}
	if r := tl.MapToMedia(1500); !r.OK || r.MediaTime != 1500 {
		t.Fatalf("identity map: %+v", r)
	}
}
