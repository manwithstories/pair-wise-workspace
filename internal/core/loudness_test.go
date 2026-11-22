package core

import (
	"math"
	"testing"

	"mediacheck/internal/model"
)

func TestLoudnessMeterFiniteAndSilence(t *testing.T) {
	m := NewLoudnessMeter(2, 48000)
	// 纯静音 1 秒。
	for i := 0; i < 48000; i++ {
		m.Push([]int32{0, 0})
	}
	st := m.Finalize()
	if math.IsNaN(st.IntegratedLUFS) || math.IsInf(st.IntegratedLUFS, 0) {
		t.Fatalf("LUFS must be finite, got %v", st.IntegratedLUFS)
	}
	if st.SilentFrames != 48000 {
		t.Fatalf("silent = %d", st.SilentFrames)
	}
	if st.IntegratedLUFS > -69 {
		t.Fatalf("silence LUFS should be near floor, got %f", st.IntegratedLUFS)
	}
}

func TestLoudnessMeterToneIsLouderThanSilence(t *testing.T) {
	m := NewLoudnessMeter(2, 48000)
	for i := 0; i < 48000*2; i++ {
		v := int32(8000 * math.Sin(2*math.Pi*440*float64(i)/48000))
		m.Push([]int32{v, v})
	}
	st := m.Finalize()
	if math.IsNaN(st.IntegratedLUFS) {
		t.Fatal("NaN in LUFS")
	}
	if st.IntegratedLUFS <= -60 {
		t.Fatalf("tone should produce measurable loudness, got %f", st.IntegratedLUFS)
	}
	if st.MeasuredFrames != 96000 {
		t.Fatalf("frames = %d", st.MeasuredFrames)
	}
}

func TestShortDurationFlagged(t *testing.T) {
	m := NewLoudnessMeter(2, 48000)
	for i := 0; i < 100; i++ {
		m.Push([]int32{100, 100})
	}
	st := m.Finalize()
	if st.ShortFrames != 100 {
		t.Fatalf("short frames = %d, want 100", st.ShortFrames)
	}
	if math.IsNaN(st.IntegratedLUFS) {
		t.Fatal("short input still needs finite LUFS")
	}
}

func TestLimiterHitsTargetAndPeak(t *testing.T) {
	l := NewLimiter(-23, 0.95)
	// 偏响素材 → 计算增益应衰减。
	l.ComputeGain(LoudnessStats{IntegratedLUFS: -10})
	if l.gainQ15 >= q15 {
		t.Fatalf("gain should attenuate, got %d", l.gainQ15)
	}
	// 提升增益（素材偏静）后，较大样本会撞到峰值上限并被限幅。
	up := NewLimiter(-14, 0.95)
	up.ComputeGain(LoudnessStats{IntegratedLUFS: -30})
	if up.gainQ15 <= q15 {
		t.Fatalf("gain should boost, got %d", up.gainQ15)
	}
	out, limited := up.Apply([]int32{32767, -32768, 100})
	if !limited {
		t.Fatal("boosted full-scale samples must be limited")
	}
	peak := up.PeakDBFS()
	if math.IsNaN(peak) || peak > -0.1 {
		t.Fatalf("peak dbfs out of range: %f", peak)
	}
	for _, v := range out {
		if math.Abs(float64(v)) > 0.95*32768 {
			t.Fatalf("sample %d exceeds peak limit", v)
		}
	}
}

func TestLimiterNoExplosionOnSilence(t *testing.T) {
	l := NewLimiter(-23, 0.95)
	l.ComputeGain(LoudnessStats{IntegratedLUFS: -70}) // 极静 → 增益封顶 6x
	out, _ := l.Apply([]int32{0, 0})
	for _, v := range out {
		if v != 0 {
			t.Fatalf("silence must stay silent after gain, got %d", v)
		}
	}
}

func TestConfigValidation(t *testing.T) {
	cfg := NormalizeConfig{InRate: 96000, TargetRate: 48000, InLayout: model.LayoutStereo, TargetLayout: model.LayoutStereo}
	if _, err := NewResampler(cfg.InRate, cfg.TargetRate); err == nil {
		t.Fatal("96k must be rejected at resampler")
	}
}
