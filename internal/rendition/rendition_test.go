package rendition

import (
	"path/filepath"
	"testing"

	"bcast/internal/fault"
	"bcast/internal/model"
	"bcast/internal/probe"
	"bcast/internal/timeline"
)

func buildTimeline(t *testing.T, durationMS int64, allFaults bool) (*timeline.Timeline, *fault.Collector) {
	t.Helper()
	dir := t.TempDir()
	tracks := probe.DefaultTracks()
	plan := model.Plan{
		Seed: 41, DurationMS: durationMS, Tracks: tracks,
		WrapModulus: probe.DefaultWrapModulus(tracks),
		Faults:      probe.DefaultFaultPlan(allFaults),
	}
	if _, err := probe.Generate(dir, plan); err != nil {
		t.Fatal(err)
	}
	fc := &fault.Collector{}
	src, err := probe.Open(filepath.Join(dir, "master.bin"), fc)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	tl, err := timeline.Build(src, fc)
	if err != nil {
		t.Fatal(err)
	}
	return tl, fc
}

func TestFanOutTiersAndSegmentRecovery(t *testing.T) {
	tl, fc := buildTimeline(t, 4000, true)
	tiers := DefaultTiers()
	rs, err := FanOut(tl, tiers, fc)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 3 {
		t.Fatalf("renditions = %d, want 3", len(rs))
	}
	for i, r := range rs {
		if r.Tier.ID != tiers[i].ID || r.Tier.Stride != tiers[i].Stride {
			t.Fatalf("rendition %d tier mismatch: %+v", i, r.Tier)
		}
		if len(r.Segments) == 0 {
			t.Fatalf("rendition %d produced no segments", i)
		}
		// 每段必须保持半开区间合法且至少含一个样本。
		for _, s := range r.Segments {
			if len(s.Samples) == 0 {
				t.Fatal("empty segment is not allowed")
			}
			if s.EndUS <= s.StartUS {
				t.Fatalf("segment half-open interval invalid: [%d,%d)", s.StartUS, s.EndUS)
			}
			for k := 1; k < len(s.Samples); k++ {
				prev, cur := s.Samples[k-1], s.Samples[k]
				if cur.PTS < prev.PTS {
					t.Fatal("segment sample PTS must be monotonic")
				}
			}
		}
	}
	// 缺口必须触发分段：含丢包的素材段数应多于轨道数（每轨至少被切一次）。
	totalSeg := 0
	for _, r := range rs {
		totalSeg += len(r.Segments)
	}
	if totalSeg <= 5 {
		t.Fatalf("drops did not split segments, total=%d", totalSeg)
	}
}

func TestFanInCleanBoundaries(t *testing.T) {
	tl, fc := buildTimeline(t, 3000, false)
	rs, err := FanOut(tl, DefaultTiers(), fc)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := FanIn(rs, fc)
	if err != nil {
		t.Fatal(err)
	}
	if rep.BoundaryMismatches != 0 {
		t.Fatalf("clean timeline boundary mismatches = %d", rep.BoundaryMismatches)
	}
	if rep.CrossLayerDeviations != 0 {
		t.Fatalf("clean timeline cross-layer deviations = %d", rep.CrossLayerDeviations)
	}
	if len(rep.PerTrack) != 5 {
		t.Fatalf("per-track spans = %d, want 5", len(rep.PerTrack))
	}
}

func TestFanInDetectsOverlap(t *testing.T) {
	// 手工构造两段重叠的表示层，核验边界错误被捕获。
	r := &Rendition{
		Tier:     Tier{ID: 0, Stride: 1},
		Span:     map[uint8][2]int64{0: {0, 200}},
		PeriodUS: map[uint8]int64{0: 33333},
		Segments: []Segment{
			{RenditionID: 0, TrackID: 0, Index: 0, StartUS: 0, EndUS: 100},
			{RenditionID: 0, TrackID: 0, Index: 1, StartUS: 80, EndUS: 200},
		},
	}
	fc := &fault.Collector{}
	rep, err := FanIn([]*Rendition{r}, fc)
	if err != nil {
		t.Fatal(err)
	}
	if rep.BoundaryMismatches != 1 {
		t.Fatalf("boundary mismatches = %d, want 1", rep.BoundaryMismatches)
	}
	if fc.CountByKind(fault.KindBoundary) != 1 {
		t.Fatalf("boundary faults = %d, want 1", fc.CountByKind(fault.KindBoundary))
	}
}

func TestConcurrentEqualsSerial(t *testing.T) {
	tl, _ := buildTimeline(t, 3000, true)
	tiers := DefaultTiers()
	fc1, fc2 := &fault.Collector{}, &fault.Collector{}
	concurrent, err := FanOut(tl, tiers, fc1)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := FanOutSerial(tl, tiers, fc2)
	if err != nil {
		t.Fatal(err)
	}
	if len(concurrent) != len(serial) {
		t.Fatalf("rendition count %d != %d", len(concurrent), len(serial))
	}
	for i := range concurrent {
		c, s := concurrent[i], serial[i]
		if len(c.Segments) != len(s.Segments) {
			t.Fatalf("tier %d segment count %d != %d", i, len(c.Segments), len(s.Segments))
		}
		for j := range c.Segments {
			cs, ss := c.Segments[j], s.Segments[j]
			if cs.StartUS != ss.StartUS || cs.EndUS != ss.EndUS ||
				cs.FirstSeq != ss.FirstSeq || cs.LastSeq != ss.LastSeq ||
				len(cs.Samples) != len(ss.Samples) {
				t.Fatalf("tier %d segment %d differs between concurrent and serial", i, j)
			}
		}
	}
	// 两路径产生的故障计数也必须一致（共享类型契约、独立收集器）。
	if fc1.CountByKind(fault.KindBoundary) != fc2.CountByKind(fault.KindBoundary) {
		t.Fatal("boundary fault counts differ between concurrent and serial")
	}
}
