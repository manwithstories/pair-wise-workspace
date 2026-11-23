package silence_test

import (
	"testing"

	"media-pipeline/internal/clock"
	"media-pipeline/internal/media"
	"media-pipeline/internal/silence"
)

var tb = clock.Timebase{SampleRate: 1000, Channels: 1, BitsPerSample: 16}

// pcm16 用给定幅度构造 n 帧单声道 16bit PCM。
func pcm16(amps ...int16) []byte {
	buf := make([]byte, len(amps)*2)
	for i, a := range amps {
		buf[i*2] = byte(a)
		buf[i*2+1] = byte(a >> 8)
	}
	return buf
}

func newDet(t *testing.T, rule silence.ChannelRule) *silence.Detector {
	t.Helper()
	d, err := silence.New(silence.Config{
		WindowFrames:     3,
		MinSilenceFrames: 3,
		PeakThreshold:    10_000, // 约满量程 30%
		RMSThreshold:     10_000,
		Channels:         1,
		BitsPerSample:   16,
		Rule:             rule,
	}, tb)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDetectsSilenceInterval(t *testing.T) {
	d := newDet(t, silence.RuleMax)
	// 4 帧响亮、6 帧静音、4 帧响亮
	loud := pcm16(30000, 30000, 30000, 30000)
	quiet := pcm16(0, 0, 0, 0, 0, 0)
	tail := pcm16(25000, 25000, 25000, 25000)

	if _, err := d.Push(media.Block{ID: "a", Format: tb, PCM: loud}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Push(media.Block{ID: "b", Format: tb, FirstSample: 4, PCM: quiet}); err != nil {
		t.Fatal(err)
	}
	ivs, err := d.Push(media.Block{ID: "c", Format: tb, FirstSample: 10, PCM: tail})
	if err != nil {
		t.Fatal(err)
	}
	ivs = append(ivs, d.Flush()...)
	if len(ivs) != 1 {
		t.Fatalf("intervals = %d, want 1: %+v", len(ivs), ivs)
	}
	iv := ivs[0]
	// 3 帧滑窗需排空历史响帧后才确认静音，起点为 6（窗口延迟，确定性行为）。
	if iv.FirstSample != 6 || iv.Frames < 3 {
		t.Fatalf("interval = %+v, want start 6 and >=3 frames", iv)
	}
	if iv.Peak > 10_000 {
		t.Fatalf("silence peak %d exceeds threshold", iv.Peak)
	}
}

func TestShortSilenceDoesNotClose(t *testing.T) {
	d := newDet(t, silence.RuleMax)
	// 仅 2 帧静音，短于 MinSilenceFrames=3，不得产生区间。
	pcm := pcm16(30000, 30000, 30000, 0, 0, 30000, 30000, 30000)
	ivs, err := d.Push(media.Block{ID: "a", Format: tb, PCM: pcm})
	if err != nil {
		t.Fatal(err)
	}
	ivs = append(ivs, d.Flush()...)
	if len(ivs) != 0 {
		t.Fatalf("short silence produced interval: %+v", ivs)
	}
}

func TestCrossBlockWindowConsistency(t *testing.T) {
	// 同一段素材：整块喂入 vs 拆成两块，静音区间必须一致。
	pcm := pcm16(20000, 20000, 20000, 0, 0, 0, 0, 0, 20000, 20000, 20000)

	run := func(parts ...media.Block) []silence.Interval {
		d := newDet(t, silence.RuleMax)
		var ivs []silence.Interval
		for _, p := range parts {
			got, err := d.Push(p)
			if err != nil {
				t.Fatal(err)
			}
			ivs = append(ivs, got...)
		}
		return append(ivs, d.Flush()...)
	}
	whole := run(media.Block{ID: "w", Format: tb, PCM: pcm})
	split := run(
		media.Block{ID: "s1", Format: tb, PCM: pcm[:6]},
		media.Block{ID: "s2", Format: tb, FirstSample: 3, PCM: pcm[6:]},
	)
	if len(whole) != len(split) {
		t.Fatalf("interval counts differ: %d vs %d", len(whole), len(split))
	}
	for i := range whole {
		if whole[i].FirstSample != split[i].FirstSample ||
			whole[i].EndSample != split[i].EndSample {
			t.Fatalf("interval %d differs: %+v vs %+v", i, whole[i], split[i])
		}
	}
}

func TestChannelAggregationRules(t *testing.T) {
	stereo := clock.Timebase{SampleRate: 1000, Channels: 2, BitsPerSample: 16}
	// 左声道静音、右声道响亮的一帧（20000/32767 ≈ 61% 满量程）。
	frame := []byte{0, 0, 0x20, 0x4e} // 0 与 20000
	cfg := silence.Config{
		WindowFrames: 1, MinSilenceFrames: 1,
		Channels: 2, BitsPerSample: 16,
	}
	// 阈值 50%：均值 RMS~43% 判静音；最大声道峰值 ~61% 判非静音。
	cfg.PeakThreshold = 500_000
	cfg.RMSThreshold = 500_000
	b := media.Block{ID: "x", Format: stereo, PCM: append(append([]byte{}, frame...), frame...)}

	dMax, _ := silence.New(cfgWithRule(cfg, silence.RuleMax), stereo)
	ivsMax, _ := dMax.Push(b)
	ivsMax = append(ivsMax, dMax.Flush()...)
	if len(ivsMax) != 0 {
		t.Fatal("RuleMax must treat frame as non-silent when one channel is loud")
	}
	dMean, _ := silence.New(cfgWithRule(cfg, silence.RuleMean), stereo)
	ivsMean, _ := dMean.Push(b)
	ivsMean = append(ivsMean, dMean.Flush()...)
	if len(ivsMean) != 1 {
		t.Fatalf("RuleMean should classify averaged frame as silent, got %+v", ivsMean)
	}
}

func cfgWithRule(c silence.Config, r silence.ChannelRule) silence.Config {
	c.Rule = r
	return c
}
