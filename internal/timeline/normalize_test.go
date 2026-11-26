package timeline

import (
	"testing"

	"archivemedia/internal/adapter"
	"archivemedia/internal/model"
)

func tb(num, den int64) model.Rational { return model.MustRational(num, den) }

func TestNormalizeBasicAndVFR(t *testing.T) {
	// 25fps 视频，时基 1/25；VFR：第 2 帧时长为 2 个刻度
	pm := &adapter.ParsedMedia{
		SourceID: "tape", AdapterName: "arc1", AdapterVersion: "1.0.0",
		Tracks: []adapter.Track{{Index: 0, Type: adapter.MediaVideo, TimeBase: tb(1, 25)}},
		Packets: []adapter.FramePacket{
			{TrackIndex: 0, StartTicks: 0, DurationTicks: 1, KeyFrame: true},
			{TrackIndex: 0, StartTicks: 1, DurationTicks: 2}, // VFR 长帧
			{TrackIndex: 0, StartTicks: 3, DurationTicks: 1},
		},
	}
	tl, err := Normalize(pm)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ start, end int64 }{
		{0, 40_000}, {40_000, 120_000}, {120_000, 160_000},
	}
	for i, f := range tl.Tracks[0].Frames {
		if f.Start.Microseconds() != want[i].start || f.End.Microseconds() != want[i].end {
			t.Fatalf("frame %d = [%d,%d] want [%d,%d]", i, f.Start, f.End, want[i].start, want[i].end)
		}
	}
	if tl.TotalDuration().Microseconds() != 160_000 {
		t.Fatalf("total duration=%d", tl.TotalDuration())
	}
}

func TestNormalizeNegativeAndNonZeroStart(t *testing.T) {
	// 音频轨 48kHz，首采样在 -48000（即 -1s），负起点必须保留为负值。
	pm := &adapter.ParsedMedia{
		Tracks: []adapter.Track{{Index: 0, Type: adapter.MediaAudio, TimeBase: tb(1, 48000)}},
		Packets: []adapter.FramePacket{
			{StartTicks: -48000, DurationTicks: 48000},
			{StartTicks: 0, DurationTicks: 48000},
		},
	}
	tl, err := Normalize(pm)
	if err != nil {
		t.Fatal(err)
	}
	f0 := tl.Tracks[0].Frames[0]
	if f0.Start.Microseconds() != -1_000_000 || f0.End.Microseconds() != 0 {
		t.Fatalf("负起点丢失: [%d,%d]", f0.Start, f0.End)
	}
	if tl.SpanStart.Microseconds() != -1_000_000 {
		t.Fatalf("统一轴 span 起点必须为负: %d", tl.SpanStart)
	}
	if tl.Tracks[0].Offset.Microseconds() != -1_000_000 {
		t.Fatalf("offset 溯源=%d", tl.Tracks[0].Offset)
	}
}

func TestNormalizeMultiTrackSingleAxis(t *testing.T) {
	// 两条不同时基的轨道必须落到同一条微秒轴，禁止各自累计。
	pm := &adapter.ParsedMedia{
		Tracks: []adapter.Track{
			{Index: 0, Type: adapter.MediaVideo, TimeBase: tb(1, 25)},
			{Index: 1, Type: adapter.MediaAudio, TimeBase: tb(1, 8000)},
		},
		Packets: []adapter.FramePacket{
			{TrackIndex: 0, StartTicks: 25, DurationTicks: 1}, // 1.000s
			{TrackIndex: 1, StartTicks: 8000, DurationTicks: 8000}, // 1.000s
		},
	}
	tl, err := Normalize(pm)
	if err != nil {
		t.Fatal(err)
	}
	v := tl.Tracks[0].Frames[0]
	a := tl.Tracks[1].Frames[0]
	if !v.Start.Equals(a.Start) || v.Start.Microseconds() != 1_000_000 {
		t.Fatalf("两轨未对齐到统一轴: v=%d a=%d", v.Start, a.Start)
	}
	all := tl.AllFrames()
	if len(all) != 2 {
		t.Fatalf("AllFrames=%d", len(all))
	}
	// 窗口查询
	if got := tl.FramesInWindow(0, model.MediaTime(900_000), model.MediaTime(1_100_000)); len(got) != 1 {
		t.Fatalf("窗口查询失败: %d", len(got))
	}
}

func TestNormalizeRejectsUnwrappedWrap(t *testing.T) {
	// 模拟回绕未展开：刻度倒退，必须报 corrupt 而非静默累计。
	pm := &adapter.ParsedMedia{
		Tracks: []adapter.Track{{Index: 0, TimeBase: tb(1, 25)}},
		Packets: []adapter.FramePacket{
			{StartTicks: 100, DurationTicks: 1},
			{StartTicks: 5, DurationTicks: 1}, // 倒退
		},
	}
	if _, err := Normalize(pm); model.KindOf(err) != model.KindCorruptMedia {
		t.Fatalf("回绕未展开应报 corrupt, got %v", err)
	}
}

func TestNormalizeRejectsBadTrackRef(t *testing.T) {
	pm := &adapter.ParsedMedia{
		Tracks: []adapter.Track{{Index: 0, TimeBase: tb(1, 25)}},
		Packets: []adapter.FramePacket{{TrackIndex: 9, StartTicks: 0}},
	}
	if _, err := Normalize(pm); model.KindOf(err) != model.KindCorruptMedia {
		t.Fatalf("越界轨引用: %v", err)
	}
}

func TestNormalizeNil(t *testing.T) {
	if _, err := Normalize(nil); model.KindOf(err) != model.KindInvalidArgument {
		t.Fatalf("nil 输入: %v", err)
	}
}
