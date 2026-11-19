package align

import (
	"testing"

	"archive.local/recover/internal/errs"
	"archive.local/recover/internal/model"
	"archive.local/recover/internal/segment"
)

// rendition builds a rendition with keyframes at the given second marks.
func rendition(name string, bitrate int, keyframeSeconds []int64, totalSeconds int64) Rendition {
	tr := &model.TrackTimeline{TrackID: 1, Kind: model.KindVideo, Timescale: 90000}
	var kf []int
	idx := map[int64]int{}
	for s := int64(0); s <= totalSeconds*25; s++ {
		ns := s * 40_000_000
		tr.PTS = append(tr.PTS, ns)
		for _, k := range keyframeSeconds {
			if k*1_000_000_000 == ns {
				idx[k] = int(s)
			}
		}
	}
	for _, k := range keyframeSeconds {
		if i, ok := idx[k]; ok {
			kf = append(kf, i)
		}
	}
	seg := segment.Build(tr, kf, segment.Config{TargetNs: 4_000_000_000, MaxBoundaryNs: 1_000_000_000})
	return Rendition{Name: name, Bitrate: bitrate, Segments: seg}
}

func TestLadderUnifiedCuts(t *testing.T) {
	src := []int64{0, 4, 8, 12}
	rends := []Rendition{
		rendition("800", 800, src, 12),
		rendition("1600", 1600, src, 12),
		rendition("3200", 3200, src, 12),
	}
	res := Ladder(rends, Config{MaxDeltaNs: 40_000_000})
	if len(res.Boundaries) != 4 {
		t.Fatalf("boundaries=%d want 4: %+v", len(res.Boundaries), res.Boundaries)
	}
	if res.MaxDeviationNs != 0 {
		t.Fatalf("delta=%d want 0", res.MaxDeviationNs)
	}
	for _, b := range res.Boundaries {
		if len(b.Actual) != 3 {
			t.Fatalf("boundary %d missing renditions: %+v", b.Index, b)
		}
	}
}

func TestLadderReportsMaxDelta(t *testing.T) {
	// The low bitrate rendition lacks the 8 s keyframe and snaps its nearest
	// cut 500 ms away.
	rends := []Rendition{
		rendition("800", 800, []int64{0, 4, 8, 12}, 12),
		rendition("1600", 1600, []int64{0, 4, 9, 12}, 12),
	}
	res := Ladder(rends, Config{MaxDeltaNs: 40_000_000})
	if res.MaxDeviationNs <= 40_000_000 {
		t.Fatalf("max delta %d should exceed limit", res.MaxDeviationNs)
	}
	if res.Pending == 0 || !res.Issues.Has(errs.CodeAlignmentDelta) {
		t.Fatalf("misalignment not reported: pending=%d issues=%v", res.Pending, res.Issues)
	}
}

func TestLadderEmptyStops(t *testing.T) {
	res := Ladder(nil, Config{})
	if !res.Issues.HasFatal() {
		t.Fatal("empty ladder should be fatal")
	}
}

func TestLadderZeroBitrateTieBreakDeterministic(t *testing.T) {
	a := rendition("same-a", 0, []int64{0, 4}, 8)
	b := rendition("same-b", 0, []int64{0, 4, 8}, 8)
	for i := 0; i < 20; i++ {
		res := Ladder([]Rendition{a, b}, Config{})
		if res.SegmentCount != 3 {
			t.Fatalf("reference selection unstable: %d", res.SegmentCount)
		}
	}
}

func TestUniqueSorted(t *testing.T) {
	rends := []Rendition{
		rendition("a", 1, []int64{0, 4, 8}, 8),
		rendition("b", 2, []int64{0, 5, 8}, 8),
	}
	got := UniqueSorted(rends)
	want := []int64{0, 4e9, 5e9, 8e9}
	if len(got) != len(want) {
		t.Fatalf("unique=%v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unique=%v want %v", got, want)
		}
	}
}
