package cut

import (
	"testing"

	"mediad/internal/clock"
	"mediad/internal/fault"
	"mediad/internal/model"
)

func manifestWith(samples []model.Sample, tb clock.Rational) *model.Manifest {
	return &model.Manifest{
		Tracks:  []model.Track{{ID: "trk-1", Timebase: tb, Timescale: uint32(tb.Num)}},
		Samples: samples,
	}
}

func healthySamples(n int) []model.Sample {
	out := make([]model.Sample, n)
	for i := range out {
		out[i] = model.Sample{
			TrackID: "trk-1", Index: uint32(i + 1),
			PTS: int64(i) * 1000, Duration: 1000,
			Size: 100, Complete: true, Keyframe: i%3 == 0,
		}
	}
	return out
}

func TestSplitHealthy(t *testing.T) {
	tb := clock.MustNew(1000, 1)
	m := manifestWith(healthySamples(9), tb)
	// 在 3s、6s 处切分，得到 3 个片段。
	segs, errs := Split(m, m.Tracks[0], "src-1",
		[]Point{{AtMs: 3000}, {AtMs: 6000}})
	if len(errs) != 0 || len(segs) != 3 {
		t.Fatalf("segs=%d errs=%v", len(segs), errs)
	}
	for i, s := range segs {
		if s.Status != model.StatusOK || len(s.Refs) != 1 {
			t.Fatalf("seg %d not ok: %+v", i, s)
		}
	}
	if segs[0].Refs[0].FirstSample != 1 || segs[0].Refs[0].LastSample != 3 {
		t.Fatalf("first segment range wrong: %+v", segs[0].Refs[0])
	}
	if segs[2].Refs[0].LastSample != 9 {
		t.Fatalf("last segment must end at sample 9: %+v", segs[2].Refs[0])
	}
}

func TestSplitMarksTruncatedAndContinues(t *testing.T) {
	tb := clock.MustNew(1000, 1)
	samples := healthySamples(9)
	samples[4].Complete = false // 第二个片段含损坏样本
	m := manifestWith(samples, tb)
	segs, errs := Split(m, m.Tracks[0], "src-1",
		[]Point{{AtMs: 3000}, {AtMs: 6000}})
	if len(segs) != 3 {
		t.Fatalf("segment count = %d", len(segs))
	}
	if segs[0].Status != model.StatusOK || segs[2].Status != model.StatusOK {
		t.Fatalf("healthy segments contaminated: %s %s", segs[0].Status, segs[2].Status)
	}
	if segs[1].Status != model.StatusTruncated {
		t.Fatalf("middle segment = %s, want truncated", segs[1].Status)
	}
	recoverable := 0
	for _, e := range errs {
		if e.Kind == fault.KindRecoverable {
			recoverable++
		}
	}
	if recoverable != 1 {
		t.Fatalf("want exactly 1 recoverable fault, got %d: %v", recoverable, errs)
	}
}

func TestSplitIncompleteGap(t *testing.T) {
	tb := clock.MustNew(1000, 1)
	m := manifestWith(healthySamples(6), tb)
	// 剪辑点落在样本之间的空隙之后且无样本覆盖：构造越界剪辑点。
	segs, errs := Split(m, m.Tracks[0], "src-1",
		[]Point{{AtMs: 9000}})
	if len(segs) != 2 || segs[1].Status != model.StatusIncomplete {
		t.Fatalf("gap segment not incomplete: %+v", segs)
	}
	if len(errs) != 1 || errs[0].Code != fault.CodeIncomplete {
		t.Fatalf("want incomplete fault, got %v", errs)
	}
}

func TestTransitionCrossTrackRef(t *testing.T) {
	tb := clock.MustNew(1000, 1)
	m := manifestWith(healthySamples(9), tb)
	other := model.Track{ID: "trk-2", Timebase: tb, Timescale: 1000}
	for i := 0; i < 9; i++ {
		m.Samples = append(m.Samples, model.Sample{
			TrackID: "trk-2", Index: uint32(i + 1),
			PTS: int64(i) * 1000, Duration: 1000, Size: 50, Complete: true,
		})
	}
	segs, _ := Split(m, m.Tracks[0], "src-1", []Point{{AtMs: 3000}})
	if err := AttachTransition(&segs[0], m, other, "src-2", 0, 3000); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if len(segs[0].Refs) != 2 {
		t.Fatalf("transition refs = %d, want 2", len(segs[0].Refs))
	}
	ref := segs[0].Refs[1]
	if ref.TrackID != "trk-2" || ref.FirstSample != 1 || ref.LastSample != 3 {
		t.Fatalf("cross-track ref wrong: %+v", ref)
	}
}

func TestSplitEmptyTrack(t *testing.T) {
	tb := clock.MustNew(1000, 1)
	m := manifestWith(nil, tb)
	segs, errs := Split(m, m.Tracks[0], "src-1", []Point{{AtMs: 1000}})
	if segs != nil || len(errs) != 1 || !fault.IsRecoverable(errs[0]) {
		t.Fatalf("empty track must yield recoverable fault: %v %v", segs, errs)
	}
}
