package audio

import (
	"math"
	"testing"

	"mediaverify/internal/fault"
)

func sine(n int, amp float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = amp * math.Sin(2*math.Pi*float64(i)/48)
	}
	return out
}

func TestMeterStats(t *testing.T) {
	var m Meter
	m.Add(sine(4800, 0.5))
	st := m.Stats()
	// RMS of sine = amp/√2 → LUFS ≈ -0.691 + 20log10(0.3536) ≈ -9.72
	want := -0.691 + 20*math.Log10(0.5/math.Sqrt2)
	if math.Abs(st.LUFS-want) > 0.01 {
		t.Fatalf("lufs=%f want=%f", st.LUFS, want)
	}
	if st.PeakDBFS < -6.03 || st.PeakDBFS > -6.0 {
		t.Fatalf("peak=%f", st.PeakDBFS)
	}
	if st.SilenceRatio != 0 {
		t.Fatalf("silence=%f", st.SilenceRatio)
	}
}

func TestMeterSilence(t *testing.T) {
	var m Meter
	m.Add(make([]float64, 1008)) // 21 个整窗静音
	m.Add(sine(1008, 0.5))
	st := m.Stats()
	if math.Abs(st.SilenceRatio-0.5) > 0.01 {
		t.Fatalf("silence=%f", st.SilenceRatio)
	}
}

func TestGainAndPeakLimit(t *testing.T) {
	var m Meter
	m.Add(sine(4800, 0.5))
	in := m.Stats()
	// 目标 -16 LUFS：增益约 -6.3dB，峰值不超限
	gain := GainToTarget(in.LUFS, -16)
	if err := CheckPeak(in, gain); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	// 目标 +20 LUFS：增益 ~+29.7dB → 峰值超限，失败但保留统计
	err := CheckPeak(in, GainToTarget(in.LUFS, 20))
	ge, ok := err.(*GainError)
	if !ok {
		t.Fatalf("err=%v", err)
	}
	fe, ok := fault.As(ge.Err)
	if !ok || fe.Kind != fault.KindLoudness {
		t.Fatalf("fe=%v", ge.Err)
	}
	if ge.Stats.PeakDBFS <= PeakLimitDBFS {
		t.Fatalf("stats not retained: %+v", ge.Stats)
	}
}

func TestApplyGain(t *testing.T) {
	s := []float64{0.5, -0.25}
	ApplyGain(s, 6.0206)
	if math.Abs(s[0]-1.0) > 0.001 || math.Abs(s[1]+0.5) > 0.001 {
		t.Fatalf("s=%v", s)
	}
}
