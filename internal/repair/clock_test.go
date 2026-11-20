package repair

import (
	"testing"

	"mediarepair/internal/model"
)

func buildTimeline(clips []model.Clip) *model.Timeline {
	return &model.Timeline{
		Tracks: []model.Track{{ID: 1, Kind: model.TrackVideo, Timescale: 90_000}},
		Clips:  clips,
	}
}

func TestDuplicateClampedMonotonic(t *testing.T) {
	tl := buildTimeline([]model.Clip{{ID: "c", TrackID: 1, StartTick: 0, DurationTick: 100_000, SampleCount: 100}})
	c := NewClock(tl, DefaultStrategy())
	seq := []uint64{0, 3000, 6000, 6000, 9000}
	var prev uint64
	for i, dts := range seq {
		rs, err := c.Process(model.Sample{TrackID: 1, Seq: uint64(i), DTS: dts, CTS: dts, Duration: 3000})
		if err != nil {
			t.Fatal(err)
		}
		if i == 3 {
			if rs.RepairFlags&model.RepairDuplicate == 0 {
				t.Fatalf("应标记重复: %v", rs.RepairFlags)
			}
			if rs.UnifiedDTS <= prev {
				t.Fatalf("重复时间戳未被钳制为递增: %d <= %d", rs.UnifiedDTS, prev)
			}
		}
		prev = rs.UnifiedDTS
	}
}

func TestRegressionClampedMonotonic(t *testing.T) {
	tl := buildTimeline([]model.Clip{{ID: "c", TrackID: 1, StartTick: 0, DurationTick: 100_000, SampleCount: 100}})
	c := NewClock(tl, DefaultStrategy())
	seq := []uint64{0, 3000, 6000, 4500, 9000}
	var prev uint64
	for i, dts := range seq {
		rs, err := c.Process(model.Sample{TrackID: 1, Seq: uint64(i), DTS: dts, CTS: dts, Duration: 3000})
		if err != nil {
			t.Fatal(err)
		}
		if i == 3 && rs.RepairFlags&model.RepairRegression == 0 {
			t.Fatalf("应标记倒退: %v", rs.RepairFlags)
		}
		if rs.UnifiedDTS <= prev && i > 0 {
			t.Fatalf("第 %d 样本未保持递增: %d <= %d", i, rs.UnifiedDTS, prev)
		}
		prev = rs.UnifiedDTS
	}
}

func TestWrapExpandsMonotonic(t *testing.T) {
	tl := buildTimeline([]model.Clip{{ID: "c", TrackID: 1, StartTick: 0, DurationTick: 10_000_000, SampleCount: 100}})
	c := NewClock(tl, DefaultStrategy())
	base := uint64(1)<<32 - 9000
	seq := []uint64{base, base + 3000, base + 6000, 0, 3000}
	var prev uint64
	sawWrap := false
	for i, dts := range seq {
		rs, err := c.Process(model.Sample{TrackID: 1, Seq: uint64(i), DTS: dts, CTS: dts, Duration: 3000})
		if err != nil {
			t.Fatal(err)
		}
		if rs.RepairFlags&model.RepairWrap != 0 {
			sawWrap = true
		}
		if i > 0 && rs.RepairedDTS <= prev {
			t.Fatalf("回绕展开后未递增: %d <= %d", rs.RepairedDTS, prev)
		}
		prev = rs.RepairedDTS
	}
	if !sawWrap {
		t.Fatal("应检测到 32 位回绕")
	}
}

func TestAuditPolicyKeepsRawValues(t *testing.T) {
	tl := buildTimeline([]model.Clip{{ID: "c", TrackID: 1, StartTick: 0, DurationTick: 100_000, SampleCount: 100}})
	c := NewClock(tl, AuditStrategy())
	var prev uint64
	for i, dts := range []uint64{0, 3000, 3000} {
		rs, err := c.Process(model.Sample{TrackID: 1, Seq: uint64(i), DTS: dts, CTS: dts, Duration: 3000})
		if err != nil {
			t.Fatal(err)
		}
		if rs.OriginalDTS != dts {
			t.Fatalf("审计模式必须保留原始 DTS")
		}
		if i == 2 && rs.RepairedDTS != prev {
			t.Fatalf("审计模式不应钳制 DTS: %d != %d", rs.RepairedDTS, prev)
		}
		prev = rs.RepairedDTS
	}
}

func TestClipJumpAtBoundary(t *testing.T) {
	tl := buildTimeline([]model.Clip{
		{ID: "a", TrackID: 1, StartTick: 0, DurationTick: 9000, FirstSample: 0, SampleCount: 3},
		{ID: "b", TrackID: 1, StartTick: 9000, DurationTick: 9000, FirstSample: 3, SampleCount: 3},
	})
	c := NewClock(tl, DefaultStrategy())
	seq := []uint64{0, 3000, 6000, 900_000, 903_000, 906_000}
	var jumps int
	for i, dts := range seq {
		rs, err := c.Process(model.Sample{TrackID: 1, Seq: uint64(i), DTS: dts, CTS: dts, Duration: 3000})
		if err != nil {
			t.Fatal(err)
		}
		if rs.RepairFlags&model.RepairClipJump != 0 {
			jumps++
			if rs.SegmentID != "b" {
				t.Fatalf("跳变应发生在剪辑 b 起点，实际 %s", rs.SegmentID)
			}
		}
	}
	if jumps != 1 {
		t.Fatalf("应检测到 1 次跨剪辑跳变，实际 %d", jumps)
	}
}

func TestDriftCorrectionChangesTicks(t *testing.T) {
	tl := buildTimeline([]model.Clip{{ID: "c", TrackID: 1, StartTick: 0, DurationTick: 1_000_000, SampleCount: 100}})
	strat := DefaultStrategy()
	strat.DriftPPM = 1000
	c := NewClock(tl, strat)
	var prev uint64
	for i := 0; i < 50; i++ {
		rs, err := c.Process(model.Sample{TrackID: 1, Seq: uint64(i), DTS: uint64(i) * 3000, CTS: uint64(i) * 3000, Duration: 3000})
		if err != nil {
			t.Fatal(err)
		}
		if i > 0 && rs.RepairedDTS <= prev {
			t.Fatalf("漂移修正后未递增")
		}
		if rs.RepairFlags&model.RepairDrift == 0 {
			t.Fatal("应标记漂移修正")
		}
		prev = rs.RepairedDTS
	}
}

func TestStatsAndSegmentsDeterministic(t *testing.T) {
	tl := buildTimeline([]model.Clip{
		{ID: "a", TrackID: 1, StartTick: 0, DurationTick: 9000, FirstSample: 0, SampleCount: 3},
		{ID: "b", TrackID: 1, StartTick: 9000, DurationTick: 9000, FirstSample: 3, SampleCount: 3},
	})
	c := NewClock(tl, DefaultStrategy())
	for i := 0; i < 6; i++ {
		if _, err := c.Process(model.Sample{TrackID: 1, Seq: uint64(i), DTS: uint64(i) * 3000, CTS: uint64(i) * 3000, Duration: 3000}); err != nil {
			t.Fatal(err)
		}
	}
	segs := c.Segments()
	if len(segs) != 2 || segs[0].ID != "a" || segs[1].ID != "b" {
		t.Fatalf("片段统计顺序错误: %+v", segs)
	}
	for _, s := range segs {
		if s.Samples != 3 {
			t.Fatalf("片段 %s 样本数 %d", s.ID, s.Samples)
		}
	}
}
