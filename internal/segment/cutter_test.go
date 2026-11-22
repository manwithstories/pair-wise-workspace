package segment

import (
	"testing"

	"mediacore/internal/clock"
	"mediacore/internal/manifest"
	"mediacore/internal/media"
	"mediacore/internal/timeline"
)

// buildTrack makes a 4-segment video track, 2s segments, 4 samples of
// 0.5s each, keyframe on sample 0 and 2 (every 1s).
func buildTrack() (*timeline.TrackTimeline, []*media.Segment) {
	tr := manifest.Track{
		TrackMeta: manifest.TrackMeta{ID: "v1", Kind: "video", Codec: "avc1", Timescale: 90000},
	}
	for i := 0; i < 4; i++ {
		tr.Segments = append(tr.Segments, manifest.Segment{
			Index: i, URI: "v.m4s", Dur: clock.Must(2, 1),
		})
	}
	pl := &manifest.Playlist{Format: "hls", Tracks: []manifest.Track{tr}}
	tl, err := timeline.Build(pl)
	if err != nil {
		panic(err)
	}
	var idx []*media.Segment
	for i := 0; i < 4; i++ {
		idx = append(idx, &media.Segment{Samples: []media.Sample{
			{Duration: 45000, Size: 10, Keyframe: true},
			{Duration: 45000, Size: 10},
			{Duration: 45000, Size: 10, Keyframe: true},
			{Duration: 45000, Size: 10},
		}})
	}
	return tl.Track("v1"), idx
}

func TestCutSnapsToKeyframe(t *testing.T) {
	tt, idx := buildTrack()
	res, err := Cut(tt, idx, Request{
		TrackID: "v1",
		Start:   clock.Must(13, 10), // 1.3s -> snaps to 1.0s
		End:     clock.Must(13, 2),  // 6.5s -> snaps to 6.0s
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Start.Actual != clock.Must(1, 1) {
		t.Fatalf("start = %v, want 1/1", res.Start.Actual)
	}
	if res.Start.DeviationMs() != 300 {
		t.Fatalf("start deviation = %v ms, want 300", res.Start.DeviationMs())
	}
	if res.End.Actual != clock.Must(6, 1) || res.End.DeviationMs() != 500 {
		t.Fatalf("end = %v dev=%v, want 6/1 500ms", res.End.Actual, res.End.DeviationMs())
	}
	if len(res.Segments) != 2 || res.Segments[0].Index != 1 || res.Segments[1].Index != 2 {
		t.Fatalf("cut segments = %+v", res.Segments)
	}
}

func TestCutExactBoundary(t *testing.T) {
	tt, idx := buildTrack()
	res, err := Cut(tt, idx, Request{TrackID: "v1", Start: clock.Must(2, 1), End: clock.Must(4, 1)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Start.DeviationMs() != 0 || res.End.DeviationMs() != 0 {
		t.Fatalf("exact boundaries should have zero deviation: %+v %+v", res.Start, res.End)
	}
}

func TestCutBeyondTrackEnd(t *testing.T) {
	tt, idx := buildTrack()
	res, err := Cut(tt, idx, Request{TrackID: "v1", Start: clock.Must(1, 1), End: clock.Must(99, 1)})
	if err != nil {
		t.Fatal(err)
	}
	if res.End.Actual != clock.Must(8, 1) {
		t.Fatalf("end should clamp to track end 8s, got %v", res.End.Actual)
	}
}

func TestCutRejectsEmptyRange(t *testing.T) {
	tt, idx := buildTrack()
	_, err := Cut(tt, idx, Request{TrackID: "v1", Start: clock.Must(2, 1), End: clock.Must(2, 1)})
	if err == nil {
		t.Fatal("empty range should fail")
	}
}

func TestCutGapBoundary(t *testing.T) {
	tt, idx := buildTrack()
	// Replace segment 1 with a gap.
	tt.Segments[1].Gap = true
	tt.Segments[1].URI = ""
	idx[1] = nil
	res, err := Cut(tt, idx, Request{TrackID: "v1", Start: clock.Must(3, 1), End: clock.Must(5, 1)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Start.Actual != clock.Must(2, 1) {
		t.Fatalf("boundary inside gap should snap to gap start 2s, got %v", res.Start.Actual)
	}
}
