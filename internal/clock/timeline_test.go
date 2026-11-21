package clock

import "testing"

func TestTimelineCleanDrift(t *testing.T) {
	tl := NewTimeline(TimelineConfig{AudioRate: 48000, IsolationThresholdNS: 20_000_000})
	// 90kHz RTP，每帧 3000 tick；音频锚点按 180ppm 漂移缓慢偏离。
	const ppm = 180.0
	var samples uint32 = 1600
	for seq := uint32(0); seq < 600; seq++ {
		tick := int64(seq) * 3000
		// 真实时间 = tick * 标称ns/tick * (1+ppm)，反推采样数。
		truthNS := float64(tick) * (1_000_000_000.0 / 90_000) * (1 + ppm/1_000_000)
		samples = uint32(truthNS/1_000_000_000*48000 + 0.5)
		tl.AddPoint(seq, tick, samples)
	}
	if err := tl.Build(); err != nil {
		t.Fatal(err)
	}
	for _, s := range tl.Segments() {
		if s.Isolated {
			t.Fatalf("正常漂移流不应出现隔离段: %+v", s)
		}
	}
	if r := tl.MaxResidual(); r > 20_000_000 {
		t.Fatalf("正常漂移流最大残差 %dns 超过 20ms", r)
	}
	ns, isolated := tl.MediaNS(300, 300*3000)
	if isolated {
		t.Fatal("中段帧不应隔离")
	}
	if ns <= 0 {
		t.Fatalf("媒体时间异常: %d", ns)
	}
}

func TestTimelineClockJumpIsolation(t *testing.T) {
	tl := NewTimeline(TimelineConfig{AudioRate: 48000, IsolationThresholdNS: 20_000_000})
	jumpAt := uint32(300)
	var samples uint32 = 1600
	for seq := uint32(0); seq < 600; seq++ {
		tick := int64(seq) * 3000
		truthNS := float64(tick) * (1_000_000_000.0 / 90_000)
		if seq >= jumpAt {
			truthNS += 500_000_000 // 500ms 时钟突变，远超 20ms 阈值
		}
		samples = uint32(truthNS/1_000_000_000*48000 + 0.5)
		tl.AddPoint(seq, tick, samples)
	}
	if err := tl.Build(); err != nil {
		t.Fatal(err)
	}
	iso := 0
	for _, s := range tl.Segments() {
		if s.Isolated {
			iso++
		}
	}
	if iso == 0 {
		t.Fatal("时钟突变必须产生隔离段")
	}
	if !tl.IsolatedAt(jumpAt) {
		t.Fatal("突变点应落在隔离段")
	}
	if tl.IsolatedAt(10) {
		t.Fatal("突变前的正常段不应被隔离")
	}
}

func TestTimelineAudioBackward(t *testing.T) {
	tl := NewTimeline(TimelineConfig{AudioRate: 48000})
	tl.AddPoint(0, 0, 4800)
	tl.AddPoint(1, 3000, 9600)
	if !tl.AddPoint(2, 6000, 1000) {
		t.Fatal("音频采样数倒退必须被识别")
	}
}
