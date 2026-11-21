package audio

import (
	"math"
	"testing"
)

func sine(rate int64, ch, frames int, amp float64) PCM {
	p := PCM{SampleRate: rate, Channels: ch, Frames: make([]float64, frames*ch)}
	for i := 0; i < frames; i++ {
		v := amp * math.Sin(2*math.Pi*440*float64(i)/float64(rate))
		for c := 0; c < ch; c++ {
			p.Frames[i*ch+c] = v
		}
	}
	return p
}

func TestResampleMonoToStereo(t *testing.T) {
	in := sine(24000, 1, 24000, 0.5) // 1s
	out, faults := Convert(in, 48000, 2)
	if len(faults) != 0 {
		t.Fatalf("faults: %v", faults)
	}
	if out.NumFrames() != 48000 || out.Channels != 2 {
		t.Fatalf("out: %d frames %d ch", out.NumFrames(), out.Channels)
	}
	// 单声道复制：左右一致
	if out.Frames[0] != out.Frames[1] {
		t.Fatal("mono duplicate")
	}
}

func TestDownmixMatrix(t *testing.T) {
	in := PCM{SampleRate: 48000, Channels: 4, Frames: []float64{1, 0, 0.5, 0.5, 1, 0, 0.5, 0.5}}
	out, _ := Convert(in, 48000, 2)
	// L = (ch0+ch2)/2 = 0.75, R = (ch1+ch3)/2 = 0.25
	if out.Frames[0] != 0.75 || out.Frames[1] != 0.25 {
		t.Fatalf("downmix: %v", out.Frames[:2])
	}
}

func TestShortTailBoundary(t *testing.T) {
	in := sine(48000, 2, 100, 0.5)
	in.Frames = append(in.Frames, 0.1) // 多 1 个采样，不足一帧
	out, faults := Convert(in, 48000, 2)
	if len(faults) != 1 || !faults[0].Recoverable {
		t.Fatalf("faults: %v", faults)
	}
	if out.NumFrames() != 100 {
		t.Fatalf("prefix kept: %d", out.NumFrames())
	}
}

func TestNormalizeOK(t *testing.T) {
	in := sine(48000, 2, 48000, 0.1)
	out, res, _ := Normalize(in, -16, -1)
	if res.Status != "ok" {
		t.Fatalf("status %s", res.Status)
	}
	if math.Abs(res.OutputLUFS-(-16)) > 0.01 {
		t.Fatalf("lufs: %v", res.OutputLUFS)
	}
	if PeakDB(out) > -1+0.01 {
		t.Fatalf("peak: %v", PeakDB(out))
	}
}

func TestNormalizeDeterministicStates(t *testing.T) {
	sil := PCM{SampleRate: 48000, Channels: 2, Frames: make([]float64, 48000)}
	_, res, f := Normalize(sil, -16, -1)
	if res.Status != "silence" || len(f) == 0 {
		t.Fatalf("silence: %+v", res)
	}
	short := sine(48000, 2, 1000, 0.5)
	_, res, _ = Normalize(short, -16, -1)
	if res.Status != "too_short" {
		t.Fatalf("short: %+v", res)
	}
	// 低 RMS 高峰值（脉冲串）：增益受峰值上限约束
	imp := PCM{SampleRate: 48000, Channels: 2, Frames: make([]float64, 96000)}
	for i := 0; i < 48000; i += 2400 {
		imp.Frames[i*2], imp.Frames[i*2+1] = 0.9, 0.9
	}
	out, res, _ := Normalize(imp, -16, -1)
	if res.Status != "gain_limited" || PeakDB(out) > -1+0.01 {
		t.Fatalf("limited: %+v peak=%v", res, PeakDB(out))
	}
}
