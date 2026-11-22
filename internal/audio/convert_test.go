package audio

import (
	"math"
	"testing"

	"mediaverify/internal/fault"
)

func TestNormalizeMonoToStereo(t *testing.T) {
	out, err := NormalizeChannels([]float64{0.5, -0.5}, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 4 || out[0] != 0.5 || out[1] != 0.5 || out[2] != -0.5 {
		t.Fatalf("out=%v", out)
	}
}

func TestNormalizeStereoToMono(t *testing.T) {
	out, err := NormalizeChannels([]float64{1, 0, 0, 1}, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0] != 0.5 || out[1] != 0.5 {
		t.Fatalf("out=%v", out)
	}
}

func TestChannelMismatch(t *testing.T) {
	_, err := NormalizeChannels([]float64{1, 2, 3}, 3, 2)
	fe, ok := fault.As(err)
	if !ok || fe.Kind != fault.KindAudio {
		t.Fatalf("err=%v", err)
	}
}

func TestNonIntegralTail(t *testing.T) {
	_, err := NormalizeChannels([]float64{1, 2, 3}, 2, 2)
	fe, ok := fault.As(err)
	if !ok || fe.Kind != fault.KindAudio {
		t.Fatalf("err=%v", err)
	}
	r, _ := NewResampler(48000, 48000, 2)
	if _, err := r.Process([]float64{1, 2, 3}); err == nil {
		t.Fatal("expected non-integral tail error")
	}
}

func TestUnsupportedRate(t *testing.T) {
	if err := CheckRate(12345); err == nil {
		t.Fatal("expected unsupported rate error")
	}
	if _, err := NewResampler(12345, 48000, 2); err == nil {
		t.Fatal("expected error")
	}
}

func TestResampleCount(t *testing.T) {
	// 48000 → 24000：960 输入帧应产生约 480 输出帧
	r, err := NewResampler(48000, 24000, 1)
	if err != nil {
		t.Fatal(err)
	}
	in := make([]float64, 960)
	for i := range in {
		in[i] = math.Sin(2 * math.Pi * float64(i) / 48)
	}
	out, err := r.Process(in)
	if err != nil {
		t.Fatal(err)
	}
	// 末帧因需下一块插值而保留：479 或 480
	if n := len(out); n < 478 || n > 480 {
		t.Fatalf("out=%d", n)
	}
	// 恒等重采样：值不变
	r2, _ := NewResampler(48000, 48000, 1)
	out2, _ := r2.Process(in)
	for i := 0; i+1 < len(out2); i++ {
		if math.Abs(out2[i]-in[i]) > 1e-9 {
			t.Fatalf("identity resample mismatch at %d: %v vs %v", i, out2[i], in[i])
		}
	}
}

func TestResampleStreamingContinuity(t *testing.T) {
	// 分两块处理与单块处理结果一致（流式边界连续）
	mk := func() []float64 {
		in := make([]float64, 200)
		for i := range in {
			in[i] = float64(i) / 200
		}
		return in
	}
	r1, _ := NewResampler(48000, 48000, 1)
	whole, _ := r1.Process(mk())
	r2, _ := NewResampler(48000, 48000, 1)
	a, _ := r2.Process(mk()[:100])
	b, _ := r2.Process(mk()[100:])
	joined := append(a, b...)
	if len(whole) != len(joined) {
		t.Fatalf("len %d vs %d", len(whole), len(joined))
	}
	for i := range whole {
		if math.Abs(whole[i]-joined[i]) > 1e-12 {
			t.Fatalf("mismatch at %d", i)
		}
	}
}
