package audio

import (
	"math"
	"testing"
)

func stereoTone(frames int, amp float32, freq float64) *Buffer {
	s := make([]float32, frames*2)
	step := 2 * math.Pi * freq / TargetRate
	for i := 0; i < frames; i++ {
		v := amp * float32(math.Sin(step*float64(i)))
		s[i*2] = v
		s[i*2+1] = v
	}
	return &Buffer{Samples: s, Frames: frames}
}

func TestMeasureLoudnessTone(t *testing.T) {
	buf := stereoTone(TargetRate*2, 0.3, 1000)
	st := MeasureLoudness(buf)
	if st.IsSilence {
		t.Fatal("正弦音不应判为静音")
	}
	if math.IsInf(st.IntegratedLUFS, -1) {
		t.Fatal("应给出有限集成响度")
	}
	// 响度应落在合理工程区间 [-40, 0] LUFS。
	if st.IntegratedLUFS < -40 || st.IntegratedLUFS > 0 {
		t.Fatalf("集成响度越界: %.2f LUFS", st.IntegratedLUFS)
	}
	if st.TruePeak <= 0 || st.TruePeakDBTP > 0 {
		t.Fatalf("真峰值异常: tp=%v dbtp=%.2f", st.TruePeak, st.TruePeakDBTP)
	}
}

func TestNormalizeHitsTarget(t *testing.T) {
	buf := stereoTone(TargetRate*3, 0.05, 880) // 偏弱信号
	res, err := Normalize(buf)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(res.StatsAfter.IntegratedLUFS-TargetLUFS) > 0.5 {
		t.Fatalf("归一后响度 %.2f，目标 %.2f，超 0.5 LU",
			res.StatsAfter.IntegratedLUFS, TargetLUFS)
	}
}

func TestTruePeakLimited(t *testing.T) {
	// 强信号归一后会触发真峰值限制。
	buf := stereoTone(TargetRate*2, 0.95, 440)
	res, err := Normalize(buf)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatsAfter.TruePeakDBTP > MaxTPdBTP+0.01 {
		t.Fatalf("真峰值 %.2f dBTP 超过 %.1f 上限",
			res.StatsAfter.TruePeakDBTP, MaxTPdBTP)
	}
	// 限制后样本不得超过满量程。
	for _, v := range buf.Samples {
		if math.Abs(float64(v)) > 1.0001 {
			t.Fatalf("出现削波: %v", v)
		}
	}
}

func TestSilenceNotAmplified(t *testing.T) {
	buf := Silence(TargetRate)
	res, err := Normalize(buf)
	if err != nil {
		t.Fatal(err)
	}
	if !res.StatsAfter.IsSilence {
		t.Fatal("静音段必须保持静音判定")
	}
	if res.GainLinear != 1 {
		t.Fatalf("静音不得放大，增益=%v", res.GainLinear)
	}
	for _, v := range buf.Samples {
		if v != 0 {
			t.Fatal("静音样本被改动")
		}
	}
}

func TestShortClipDeterministic(t *testing.T) {
	// 不足一个 400ms 块的短片必须有确定结果。
	mk := func() *Buffer { return stereoTone(2000, 0.2, 660) }
	r1, err := Normalize(mk())
	if err != nil {
		t.Fatal(err)
	}
	r2, err := Normalize(mk())
	if err != nil {
		t.Fatal(err)
	}
	if r1.GainLinear != r2.GainLinear ||
		r1.StatsAfter.IntegratedLUFS != r2.StatsAfter.IntegratedLUFS {
		t.Fatal("短片归一结果不确定")
	}
	if math.IsInf(r1.StatsAfter.IntegratedLUFS, 0) {
		t.Fatal("短片必须给出有限响度")
	}
}

func TestIncompleteTailPackets(t *testing.T) {
	// 模拟不完整尾包：非整块、长度任意。
	buf := stereoTone(12345, 0.15, 330)
	res, err := Normalize(buf)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(res.StatsAfter.IntegratedLUFS-TargetLUFS) > 0.5 {
		t.Fatalf("尾包归一偏差 %.2f LU",
			math.Abs(res.StatsAfter.IntegratedLUFS-TargetLUFS))
	}
}

func TestLoudnessDeterministicAcrossRuns(t *testing.T) {
	measure := func() float64 {
		return MeasureLoudness(stereoTone(TargetRate, 0.25, 500)).IntegratedLUFS
	}
	if measure() != measure() {
		t.Fatal("响度测量不确定")
	}
}

func TestEmptyAndNilBuffer(t *testing.T) {
	empty := MeasureLoudness(&Buffer{})
	if !math.IsInf(empty.IntegratedLUFS, -1) {
		t.Fatal("空缓冲响度应为 -Inf")
	}
	if _, err := Normalize(nil); err == nil {
		t.Fatal("nil 缓冲必须报错")
	}
}
