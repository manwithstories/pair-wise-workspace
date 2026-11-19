package segment

import (
	"testing"

	"archive.local/recover/internal/errs"
	"archive.local/recover/internal/model"
)

// track builds a 25 fps video timeline of n samples starting at zero with a
// keyframe every kfEvery samples.
func track(id uint32, n, kfEvery int) (*model.TrackTimeline, []int) {
	tr := &model.TrackTimeline{TrackID: id, Kind: model.KindVideo, Timescale: 90000}
	var kf []int
	for i := 0; i < n; i++ {
		tr.PTS = append(tr.PTS, int64(i)*40_000_000)
		if i%kfEvery == 0 {
			tr.Keyframes = append(tr.Keyframes, i)
			kf = append(kf, i)
		}
	}
	return tr, kf
}

func TestBuildCutsOnKeyframes(t *testing.T) {
	tr, kf := track(1, 300, 25) // 12 s, keyframe every second
	res := Build(tr, kf, Config{TargetNs: 4_000_000_000})
	if res.SegmentCount != 3 {
		t.Fatalf("segments=%d want 3: %+v", res.SegmentCount, res.Cuts)
	}
	for _, c := range res.Cuts {
		found := false
		for _, k := range kf {
			if k == c.KeyframeSample {
				found = true
			}
		}
		if !found {
			t.Fatalf("cut not on keyframe: %+v", c)
		}
		if c.DeviationNs != 0 {
			t.Fatalf("exact keyframe reported nonzero deviation: %+v", c)
		}
	}
	if res.MaxDeviationNs != 0 {
		t.Fatalf("max deviation=%d", res.MaxDeviationNs)
	}
	// Segments must tile the timeline without gaps.
	for i := 1; i < len(res.Cuts); i++ {
		if res.Cuts[i].StartNs != res.Cuts[i-1].EndNs {
			t.Fatalf("gap between segments %d and %d", i-1, i)
		}
	}
}

func TestBuildReportsBoundaryDeviation(t *testing.T) {
	tr := &model.TrackTimeline{TrackID: 1, Kind: model.KindVideo, Timescale: 90000}
	// Keyframes only at 0 and 10 s; target 4 s cannot be met.
	var kf []int
	for i := 0; i < 300; i++ {
		tr.PTS = append(tr.PTS, int64(i)*40_000_000)
		if i == 0 || i == 250 {
			kf = append(kf, i)
		}
	}
	res := Build(tr, kf, Config{TargetNs: 4_000_000_000, MaxBoundaryNs: 40_000_000})
	if res.Pending == 0 {
		t.Fatal("large deviation not flagged pending")
	}
	if !res.Issues.Has(errs.CodeSegmentBoundary) {
		t.Fatalf("deviation not reported: %v", res.Issues)
	}
	if res.MaxDeviationNs < 5_900_000_000 {
		t.Fatalf("reported deviation %d ns too small", res.MaxDeviationNs)
	}
}

func TestBuildFirstSampleNotKeyframe(t *testing.T) {
	tr, _ := track(1, 100, 25)
	// Pretend the first keyframe is at sample 10.
	res := Build(tr, []int{10, 25, 50, 75}, Config{TargetNs: 1_000_000_000})
	if !res.Issues.Has(errs.CodeIndexPendingReview) {
		t.Fatalf("missing leading-gap review: %v", res.Issues)
	}
	if res.Cuts[0].KeyframeSample != 10 {
		t.Fatalf("first cut=%d want 10", res.Cuts[0].KeyframeSample)
	}
}

func TestBuildNoKeyframesStops(t *testing.T) {
	tr, _ := track(1, 10, 1)
	res := Build(tr, nil, Config{})
	if !res.Issues.HasFatal() {
		t.Fatal("missing keyframe dependency should be fatal")
	}
	if res.SegmentCount != 0 {
		t.Fatal("segments produced without keyframes")
	}
}

func TestBuildIgnoresOutOfRangeKeyframes(t *testing.T) {
	tr, kf := track(1, 100, 25)
	res := Build(tr, append(kf, 5000), Config{TargetNs: 1_000_000_000})
	if !res.Issues.Has(errs.CodeIndexOffsetOutOfRange) {
		t.Fatalf("out-of-range keyframe not reported: %v", res.Issues)
	}
	if res.SegmentCount == 0 {
		t.Fatal("valid keyframes were discarded with the invalid one")
	}
}

func TestBuildVFRSegmentsStillTile(t *testing.T) {
	tr := &model.TrackTimeline{TrackID: 1, Kind: model.KindVideo, Timescale: 90000}
	gaps := []int64{40e6, 40e6, 50e6, 33e6, 60e6, 40e6}
	var t0 int64
	var kf []int
	for i := 0; i < 120; i++ {
		g := gaps[i%len(gaps)]
		tr.PTS = append(tr.PTS, t0)
		if i%10 == 0 {
			kf = append(kf, i)
		}
		t0 += g
	}
	res := Build(tr, kf, Config{TargetNs: 2_000_000_000, MaxBoundaryNs: 100_000_000})
	if res.SegmentCount < 2 {
		t.Fatalf("segments=%d", res.SegmentCount)
	}
	for i := 1; i < len(res.Cuts); i++ {
		if res.Cuts[i].StartNs != res.Cuts[i-1].EndNs {
			t.Fatalf("VFR segments do not tile at %d", i)
		}
		if res.Cuts[i].StartNs <= res.Cuts[i-1].StartNs {
			t.Fatalf("segment starts not increasing at %d", i)
		}
	}
}

func TestBuildDuplicateKeyframesSafe(t *testing.T) {
	tr, kf := track(1, 120, 10)
	dup := append(append([]int{}, kf...), kf...)
	res := Build(tr, dup, Config{TargetNs: 1_000_000_000})
	for i := 1; i < len(res.Cuts); i++ {
		if res.Cuts[i].StartNs <= res.Cuts[i-1].StartNs {
			t.Fatalf("duplicate keyframes produced non-increasing starts: %+v", res.Cuts)
		}
	}
}

func TestBuildTinyTrackStillTiles(t *testing.T) {
	tr, kf := track(1, 2, 1)
	res := Build(tr, kf, Config{TargetNs: 4_000_000_000})
	if res.SegmentCount != 1 {
		t.Fatalf("segments=%d want 1", res.SegmentCount)
	}
	if res.Cuts[0].EndNs <= res.Cuts[0].StartNs {
		t.Fatalf("degenerate cut: %+v", res.Cuts[0])
	}
}

// TestBuildDoesNotStopAfterAnOvershoot reproduces a case where the first cut
// overshoots the second target: a naive cursor then rejects the only
// candidate and stops segmenting early.
func TestBuildDoesNotStopAfterAnOvershoot(t *testing.T) {
	tr := &model.TrackTimeline{TrackID: 1, Kind: model.KindVideo, Timescale: 90000}
	for i := 0; i < 200; i++ {
		tr.PTS = append(tr.PTS, int64(i)*40_000_000)
	}
	// Keyframes: 0 s, 2.48 s, 3.0 s, 5.0 s.
	kf := []int{0, 62, 75, 125}
	res := Build(tr, kf, Config{TargetNs: 1_000_000_000, MaxBoundaryNs: 2_000_000_000})
	if res.SegmentCount < 4 {
		t.Fatalf("segments=%d, tail after the overshoot was dropped: %+v", res.SegmentCount, res.Cuts)
	}
	// Every keyframe that can start a segment must have been used.
	used := map[int]bool{}
	for _, c := range res.Cuts {
		used[c.KeyframeSample] = true
	}
	for _, k := range kf {
		if !used[k] {
			t.Fatalf("keyframe sample %d was skipped: %+v", k, res.Cuts)
		}
	}
}
