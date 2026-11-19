package audio

import (
	"math"
	"testing"

	"avpipe/internal/fault"
)

func sineBlock(rate, ch, frames int, hz, amp float64, startFrame int64) Block {
	b := Block{StartFrame: startFrame, SampleRate: rate, Frames: frames,
		Layout: Layout{Name: "mono", Channels: []Channel{ChFC}},
		Data:   make([]float64, frames*ch)}
	for f := 0; f < frames; f++ {
		v := amp * math.Sin(2*math.Pi*hz*float64(startFrame+int64(f))/float64(rate))
		for c := 0; c < ch; c++ {
			b.Data[f*ch+c] = v
		}
	}
	return b
}

func runResampler(t *testing.T, src, dst, total, block int) ([]float64, *Resampler) {
	t.Helper()
	layout, _ := LayoutForChannels(1)
	r, err := NewResampler(ResamplerConfig{SrcRate: src, DstRate: dst, Layout: layout})
	if err != nil {
		t.Fatal(err)
	}
	var out []float64
	for start := 0; start < total; start += block {
		n := block
		if start+n > total {
			n = total - start
		}
		if err := r.Push(sineBlock(src, 1, n, 1000, 0.5, int64(start))); err != nil {
			t.Fatal(err)
		}
		for _, b := range r.Slurp(false, 256) {
			out = append(out, b.Data...)
		}
	}
	for _, b := range r.Slurp(true, 256) {
		out = append(out, b.Data...)
	}
	return out, r
}

func TestResampleChunkingInvariance(t *testing.T) {
	coarse, _ := runResampler(t, 48000, 44100, 48000, 4096)
	fine, _ := runResampler(t, 48000, 44100, 48000, 137)
	if len(coarse) != len(fine) {
		t.Fatalf("length depends on chunking: %d vs %d", len(coarse), len(fine))
	}
	for i := range coarse {
		if math.Abs(coarse[i]-fine[i]) > 1e-12 {
			t.Fatalf("sample %d differs by chunking: %v vs %v", i, coarse[i], fine[i])
		}
	}
}

func TestResampleOutputLengthAndPassband(t *testing.T) {
	out, r := runResampler(t, 48000, 24000, 48000, 1024)
	want := 24000
	if math.Abs(float64(len(out)-want)) > 2 {
		t.Fatalf("output %d frames, want about %d", len(out), want)
	}
	// 1 kHz tone must survive the decimation at roughly its input amplitude.
	var peak float64
	for i := 200; i < len(out)-200; i++ {
		if a := math.Abs(out[i]); a > peak {
			peak = a
		}
	}
	if math.Abs(peak-0.5) > 0.02 {
		t.Fatalf("passband peak %v, want about 0.5", peak)
	}
	// The 48k -> 24k ratio is exact, so the output length is exact.
	if len(out) != want {
		t.Fatalf("output %d frames, want exactly %d", len(out), want)
	}
	if r.RoundingError() >= 1 {
		t.Fatalf("rounding error %v exceeds one sample", r.RoundingError())
	}
}

func TestResampleFlushEmitsTail(t *testing.T) {
	layout, _ := LayoutForChannels(1)
	r, _ := NewResampler(ResamplerConfig{SrcRate: 44100, DstRate: 48000, Layout: layout})
	if err := r.Push(sineBlock(44100, 1, 512, 440, 0.3, 0)); err != nil {
		t.Fatal(err)
	}
	before := 0
	for _, b := range r.Slurp(false, 64) {
		before += b.Frames
	}
	after := 0
	for _, b := range r.Slurp(true, 64) {
		after += b.Frames
	}
	if after == 0 {
		t.Fatal("flush produced no tail samples")
	}
	// Ideal ratio: 512 * 48000/44100 = 557.27; the flush rounds up to 558
	// so the filter tail is not clipped.
	if got := before + after; got != 558 {
		t.Fatalf("total frames %d, want 558", got)
	}
	if again := r.Slurp(true, 64); len(again) != 0 {
		t.Fatalf("second flush emitted %d blocks", len(again))
	}
}

func TestResampleRejectsMismatchedRate(t *testing.T) {
	layout, _ := LayoutForChannels(1)
	r, _ := NewResampler(ResamplerConfig{SrcRate: 48000, DstRate: 48000, Layout: layout})
	err := r.Push(sineBlock(44100, 1, 64, 440, 0.3, 0))
	if e := fault.Root(err); e == nil || e.Code != fault.CodeResample {
		t.Fatalf("want resample error, got %v", err)
	}
	if _, err := NewResampler(ResamplerConfig{SrcRate: 48000, DstRate: 48000,
		Layout: layout, Taps: 8}); err == nil {
		t.Fatal("even tap count accepted")
	}
}

func TestResampleDeterministicAcrossRuns(t *testing.T) {
	a, _ := runResampler(t, 44100, 48000, 20000, 333)
	b, _ := runResampler(t, 44100, 48000, 20000, 333)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("nondeterministic output at %d", i)
		}
	}
}
