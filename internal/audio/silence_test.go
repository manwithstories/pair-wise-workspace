package audio

import (
	"math"
	"testing"

	"archivemedia/internal/model"
)

func TestRmsDbMath(t *testing.T) {
	// 满量程单声道恒定样本：32767 -> 约 -0.0003 dBFS，接近 0
	samples := []int16{32767, 32767}
	if got := rmsDb(samples, 1, 0, 2); got >= 0 || got < -0.01 {
		t.Fatalf("full-scale rms=%f", got)
	}
	// 纯静音 -> -Inf
	if got := rmsDb([]int16{0, 0, 0, 0}, 1, 0, 4); !math.IsInf(got, -1) {
		t.Fatalf("silence rms=%f want -Inf", got)
	}
	// 立体声交织：两声道各两个样本
	stereo := []int16{100, 200, 100, 200}
	got := rmsDb(stereo, 2, 0, 2)
	want := 20 * math.Log10(math.Sqrt((100*100+200*200+100*100+200*200)/4.0)/32768)
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("stereo rms=%f want %f", got, want)
	}
}

func TestDetectSilenceAndThreshold(t *testing.T) {
	// 8kHz 单声道，窗 800 样本（100ms）。前 200ms 静音，后 100ms 满幅。
	cfg := Config{WindowSamples: 800, ThresholdDb: -40, MinDuration: model.Duration(50_000)}
	silent := make([]int16, 1600)
	loud := make([]int16, 800)
	for i := range loud {
		loud[i] = 20000 // 约 -4.3 dBFS，高于阈值
	}
	frames := []Frame{
		{TrackIndex: 0, Start: 0, SampleRate: 8000, Channels: 1, Samples: silent},
		{TrackIndex: 0, Start: model.MediaTime(200_000), SampleRate: 8000, Channels: 1, Samples: loud},
	}
	ivs, err := Detect(frames, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(ivs) != 1 {
		t.Fatalf("静音区间数=%d", len(ivs))
	}
	iv := ivs[0]
	if iv.Start.Microseconds() != 0 || iv.End.Microseconds() != 200_000 {
		t.Fatalf("区间=[%d,%d]", iv.Start, iv.End)
	}
	if !math.IsInf(iv.MinRmsDb, -1) || !iv.Complete {
		t.Fatalf("rms=%v complete=%v", iv.MinRmsDb, iv.Complete)
	}
}

func TestCrossBlockMerge(t *testing.T) {
	// 静音被切成两块，每块 100ms，中间无音频间隔 -> 必须合并成 200ms。
	cfg := Config{WindowSamples: 400, ThresholdDb: -40, MinDuration: 0}
	block := make([]int16, 400)
	frames := []Frame{
		{TrackIndex: 3, Start: 0, SampleRate: 4000, Channels: 1, Samples: block},
		{TrackIndex: 3, Start: model.MediaTime(100_000), SampleRate: 4000, Channels: 1, Samples: block},
	}
	ivs, err := Detect(frames, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(ivs) != 1 {
		t.Fatalf("跨块静音未合并: %d 个区间", len(ivs))
	}
	if ivs[0].Duration.Microseconds() != 200_000 || ivs[0].TrackIndex != 3 {
		t.Fatalf("合并区间=%+v", ivs[0])
	}
}

func TestMinDurationFilter(t *testing.T) {
	// 仅 50ms 静音，要求 >=100ms -> 被过滤。
	cfg := Config{WindowSamples: 200, ThresholdDb: -40,
		MinDuration: model.Duration(100_000)}
	frames := []Frame{{Start: 0, SampleRate: 4000, Channels: 1, Samples: make([]int16, 200)}}
	ivs, err := Detect(frames, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(ivs) != 0 {
		t.Fatalf("短静音应被过滤: %+v", ivs)
	}
}

func TestIncompleteChannelFrame(t *testing.T) {
	// 立体声但样本数不能被 2 整除 -> 缺帧，区间必须标记不完整。
	cfg := Config{WindowSamples: 4, ThresholdDb: -40, MinDuration: 0}
	bad := []int16{0, 0, 0, 0, 0, 0, 0} // 7 个样本，无法构成完整立体声帧
	frames := []Frame{{Start: 0, SampleRate: 8000, Channels: 2, Samples: bad}}
	ivs, err := Detect(frames, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(ivs) == 0 {
		t.Fatal("应仍产出静音区间")
	}
	if ivs[0].Complete {
		t.Fatal("声道缺帧必须标记 Complete=false")
	}
}

func TestDetectorRejectsBadConfig(t *testing.T) {
	if _, err := NewDetector(Config{WindowSamples: 0, ThresholdDb: -40}); err == nil {
		t.Fatal("窗长 0 必须报错")
	}
	if _, err := NewDetector(Config{WindowSamples: 8, ThresholdDb: math.NaN()}); err == nil {
		t.Fatal("NaN 阈值必须报错")
	}
	if err := (&Detector{}).Push(Frame{}); err == nil {
		t.Fatal("非法采样率帧必须报错")
	}
}
