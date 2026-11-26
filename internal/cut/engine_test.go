package cut

import (
	"testing"

	"archivemedia/internal/adapter"
	"archivemedia/internal/model"
	"archivemedia/internal/timeline"
)

func buildTimeline(t *testing.T) *timeline.Timeline {
	t.Helper()
	// 视频轨 25fps（40ms/帧，10 帧 = 400ms）；音频轨 8kHz（125us/采样）
	pm := &adapter.ParsedMedia{
		Tracks: []adapter.Track{
			{Index: 0, Type: adapter.MediaVideo, TimeBase: model.MustRational(1, 25)},
			{Index: 1, Type: adapter.MediaAudio, TimeBase: model.MustRational(1, 8000)},
		},
	}
	for i := 0; i < 10; i++ {
		pm.Packets = append(pm.Packets, adapter.FramePacket{
			TrackIndex: 0, StartTicks: int64(i), DurationTicks: 1, KeyFrame: i == 0,
		})
	}
	// 音频 0..400ms，每块 1000 采样（125ms 一块不够整齐，用逐采样刻度 800 个）
	for i := 0; i < 3200; i++ {
		pm.Packets = append(pm.Packets, adapter.FramePacket{
			TrackIndex: 1, StartTicks: int64(i), DurationTicks: 1,
		})
	}
	tl, err := timeline.Normalize(pm)
	if err != nil {
		t.Fatal(err)
	}
	return tl
}

func TestCutRoundingAndError(t *testing.T) {
	e := NewEngine(buildTimeline(t))
	// 剪辑点 50ms 落在 40ms 与 80ms 帧之间，nearest -> 40ms，误差 -10ms
	segs, err := e.Cut([]Range{{
		TrackIndex: 0,
		Start:      CutPoint{At: model.MediaTime(50_000), Mode: model.RoundNearest},
		End:        CutPoint{At: model.MediaTime(200_000), Mode: model.RoundNearest},
	}})
	if err != nil {
		t.Fatal(err)
	}
	s := segs[0]
	if s.Start.Microseconds() != 40_000 || s.End.Microseconds() != 200_000 {
		t.Fatalf("snap 失败: [%d,%d]", s.Start, s.End)
	}
	if s.Roundings[0].ErrorUs != -10_000 {
		t.Fatalf("取整误差未留痕: %+v", s.Roundings[0])
	}
	if s.Duration.Microseconds() != 160_000 {
		t.Fatalf("duration=%d", s.Duration)
	}
}

func TestCutFloorCeil(t *testing.T) {
	e := NewEngine(buildTimeline(t))
	segs, err := e.Cut([]Range{{
		TrackIndex: 0,
		Start: CutPoint{At: model.MediaTime(50_000), Mode: model.RoundFloor}, // ->40ms
		End:   CutPoint{At: model.MediaTime(70_000), Mode: model.RoundCeil},  // ->80ms
	}})
	if err != nil {
		t.Fatal(err)
	}
	if segs[0].Start.Microseconds() != 40_000 || segs[0].End.Microseconds() != 80_000 {
		t.Fatalf("floor/ceil=[%d,%d]", segs[0].Start, segs[0].End)
	}
}

func TestCutIllegalPoints(t *testing.T) {
	e := NewEngine(buildTimeline(t))
	cases := []Range{
		{TrackIndex: 9, Start: CutPoint{At: 0}, End: CutPoint{At: 100_000}}, // 越界轨
		{TrackIndex: 0, Start: CutPoint{At: 200_000}, End: CutPoint{At: 100_000}}, // 倒置
		{TrackIndex: 0, Start: CutPoint{At: -5_000_000}, End: CutPoint{At: 0}},    // 超出 span
	}
	for i, r := range cases {
		if _, err := e.Cut([]Range{r}); model.KindOf(err) != model.KindInvalidArgument {
			t.Fatalf("case %d 期望 invalid_argument, got %v", i, err)
		}
	}
}

func TestTransitionCompositeNoDoubleTiming(t *testing.T) {
	e := NewEngine(buildTimeline(t))
	// 两轨在 [40ms,200ms) 转场；音频刻度 125us，边界 40000/200000 可整除
	specs := []TransitionSpec{{
		TrackA: 0, TrackB: 1,
		Start: model.MediaTime(40_000), End: model.MediaTime(200_000),
		Mode: model.RoundNearest,
	}}
	segs, err := e.ComposeTransitions(specs)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 1 || segs[0].Kind != KindTransition {
		t.Fatalf("复合段形态错误: %+v", segs)
	}
	// 并集只计时一次：160ms，而不是两成员相加 320ms
	if segs[0].Duration.Microseconds() != 160_000 {
		t.Fatalf("转场重复计时: duration=%d", segs[0].Duration)
	}
	if len(segs[0].Members) != 2 {
		t.Fatalf("复合段成员数=%d", len(segs[0].Members))
	}
	if TotalTimedMicros(segs) != 160_000 {
		t.Fatalf("总计时=%d 应等于单段", TotalTimedMicros(segs))
	}
}

func TestDuplicateTransitionDedup(t *testing.T) {
	e := NewEngine(buildTimeline(t))
	sp := TransitionSpec{TrackA: 0, TrackB: 1,
		Start: model.MediaTime(40_000), End: model.MediaTime(120_000)}
	segs, err := e.ComposeTransitions([]TransitionSpec{sp, sp, sp})
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 1 {
		t.Fatalf("重复转场未去重: %d 段", len(segs))
	}
	if segs[0].TransitionDedup != 2 {
		t.Fatalf("去重计数=%d 应为 2（额外出现次数）", segs[0].TransitionDedup)
	}
}

func TestTransitionBadSpec(t *testing.T) {
	e := NewEngine(buildTimeline(t))
	if _, err := e.ComposeTransitions([]TransitionSpec{{
		TrackA: 0, TrackB: 1, Start: 200_000, End: 100_000,
	}}); model.KindOf(err) != model.KindInvalidArgument {
		t.Fatalf("倒置转场: %v", err)
	}
}
