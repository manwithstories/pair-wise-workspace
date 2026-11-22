package core

import (
	"testing"

	"mediacheck/internal/model"
)

func TestResamplerUnsupportedRate(t *testing.T) {
	if _, err := NewResampler(96000, 48000); err == nil {
		t.Fatal("unsupported rate must fail")
	} else if me, ok := model.AsMediaError(err); !ok || me.Category != model.ErrUnsupportedRate {
		t.Fatalf("want unsupported_sample_rate, got %v", err)
	}
}

func TestResamplerPassthrough(t *testing.T) {
	rs, _ := NewResampler(48000, 48000)
	in := []int32{1, -2, 3, -4}
	out := rs.Process(in)
	if len(out) != len(in) {
		t.Fatalf("passthrough len = %d", len(out))
	}
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("passthrough mutated sample at %d", i)
		}
	}
}

func TestResampler441To48FrameCount(t *testing.T) {
	// 44100 个输入帧（1 秒）应输出 48000 帧，误差 ≤1。
	in := make([]int32, 44100)
	for i := range in {
		in[i] = int32((i * 137) % 2000 - 1000)
	}
	rs, _ := NewResampler(44100, 48000)
	out := rs.Process(in)
	want := ExpectedOutputFrames(44100, 44100, 48000)
	diff := int64(len(out)) - want
	if diff < 0 {
		diff = -diff
	}
	if diff > 1 {
		t.Fatalf("output frames=%d expected=%d delta=%d", len(out), want, diff)
	}
	if want != 48000 {
		t.Fatalf("rational expected = %d, want 48000", want)
	}
}

func TestResamplerNoNaNOrExplosion(t *testing.T) {
	// 极端幅值输入重采样后必须仍在 int32 范围且插值不越界。
	in := make([]int32, 1000)
	for i := range in {
		if i%2 == 0 {
			in[i] = 32767
		} else {
			in[i] = -32768
		}
	}
	rs, _ := NewResampler(44100, 48000)
	out := rs.Process(in)
	for _, v := range out {
		if v > 32767 || v < -32768 {
			t.Fatalf("interpolated sample out of int16 range: %d", v)
		}
	}
}

func TestResamplerMultiChunkPhaseContinuity(t *testing.T) {
	// 分块处理与一次性处理的输出帧数必须一致（相位连续，不重复/丢帧）。
	full := make([]int32, 44100)
	for i := range full {
		full[i] = int32(i % 97)
	}
	rsOne, _ := NewResampler(44100, 48000)
	one := rsOne.Process(full)

	rsChunk, _ := NewResampler(44100, 48000)
	var chunked []int32
	for off := 0; off < len(full); off += 1000 {
		end := off + 1000
		if end > len(full) {
			end = len(full)
		}
		chunked = append(chunked, rsChunk.Process(full[off:end])...)
	}
	if len(chunked) != len(one) {
		t.Fatalf("chunked=%d one-shot=%d phase not continuous", len(chunked), len(one))
	}
}
