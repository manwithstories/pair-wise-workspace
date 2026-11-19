package audio

import (
	"math"
	"testing"

	"mediapipeline/internal/mediaerr"
)

func TestResampleDownsampleBlockCounts(t *testing.T) {
	r, merr := NewResampler(48000, 24000)
	if merr != nil {
		t.Fatal(merr)
	}
	block := make([]float64, 480)
	for i := range block {
		block[i] = 0.5
	}
	out, merr := r.Process(block)
	if merr != nil {
		t.Fatal(merr)
	}
	if len(out) != 240 {
		t.Fatalf("downsample block=%d want 240", len(out))
	}
	tail, merr := r.Flush()
	if merr != nil {
		t.Fatal(merr)
	}
	// The output grid stops at t=239 while the final input is at t=479,
	// so Flush emits one endpoint-clamped tail sample.
	if len(tail) != 1 || tail[0] != 0.5 {
		t.Fatalf("expected one clamped tail sample, got %v", tail)
	}
}

func TestResampleUpsample(t *testing.T) {
	r, _ := NewResampler(24000, 48000)
	block := make([]float64, 240)
	out, merr := r.Process(block)
	if merr != nil {
		t.Fatal(merr)
	}
	if len(out) != 480 {
		t.Fatalf("upsample block=%d want 480", len(out))
	}
}

func TestCrossBlockStateConstantSignal(t *testing.T) {
	r, _ := NewResampler(48000, 8000)
	first, _ := r.Process(makeConst(100, 0.75))
	second, _ := r.Process(makeConst(100, 0.75))
	tail, _ := r.Flush()
	all := append(append(first, second...), tail...)
	if len(all) == 0 {
		t.Fatal("no output produced")
	}
	for i, v := range all {
		if math.Abs(v-0.75) > 1e-12 {
			t.Fatalf("sample %d=%v breaks constant signal", i, v)
		}
	}
}

func TestResampleFlushAndReuse(t *testing.T) {
	// Ratio 3:2 (input-time = n*3/2): a 3-sample block emits outputs at
	// t=0 and t=1.5; the final input sample at t=2 is never reached by the
	// grid, so Flush emits one endpoint-clamped tail.
	r, _ := NewResampler(3, 2)
	out, merr := r.Process([]float64{0.1, 0.2, 0.3})
	if merr != nil || len(out) != 2 {
		t.Fatalf("process output wrong: %v err=%v", out, merr)
	}
	tail, merr := r.Flush()
	if merr != nil || len(tail) != 1 || tail[0] != 0.3 {
		t.Fatalf("flush tail wrong: tail=%v err=%v", tail, merr)
	}
	if _, merr := r.Flush(); merr == nil {
		t.Fatal("double flush must error")
	}
	if _, merr := r.Process([]float64{1}); merr == nil {
		t.Fatal("process after flush must error until reset")
	}
	r.Reset()
	out, merr = r.Process([]float64{1, 2, 3})
	if merr != nil || len(out) == 0 {
		t.Fatalf("reset reuse failed: out=%d err=%v", len(out), merr)
	}
}

func TestEmptyStreamFlush(t *testing.T) {
	r, _ := NewResampler(48000, 24000)
	tail, merr := r.Flush()
	if merr != nil || len(tail) != 0 {
		t.Fatalf("empty stream must flush to empty slice: tail=%v err=%v", tail, merr)
	}
}

func TestEmptyAndShortBlocks(t *testing.T) {
	r, _ := NewResampler(48000, 24000)
	out, merr := r.Process(nil)
	if merr != nil || len(out) != 0 {
		t.Fatalf("empty block must be no-op: out=%d err=%v", len(out), merr)
	}
	out, merr = r.Process([]float64{})
	if merr != nil || len(out) != 0 {
		t.Fatalf("zero-length block must be no-op: out=%d err=%v", len(out), merr)
	}
	// A single-sample block still yields one output and a flushable tail.
	one, merr := r.Process([]float64{0.9})
	if merr != nil || len(one) != 1 || one[0] != 0.9 {
		t.Fatalf("single sample block wrong: %v err=%v", one, merr)
	}
}

func TestResampleInterpolationValue(t *testing.T) {
	// 2:1 upsampling of a ramp: positions t=0,0.5,1,1.5 with edge clamping.
	r, _ := NewResampler(1000, 2000)
	out, merr := r.Process([]float64{0, 1})
	if merr != nil {
		t.Fatal(merr)
	}
	want := []float64{0, 0.5, 1, 1}
	if len(out) != len(want) {
		t.Fatalf("interpolation length wrong: %v", out)
	}
	for i := range want {
		if math.Abs(out[i]-want[i]) > 1e-12 {
			t.Fatalf("interpolation wrong: got %v want %v", out, want)
		}
	}
}

func TestInvalidRates(t *testing.T) {
	if _, merr := NewResampler(0, 48000); merr == nil || merr.Kind != mediaerr.KindInvalidInput {
		t.Fatal("zero input rate must error")
	}
	if _, merr := NewResampler(48000, -1); merr == nil {
		t.Fatal("negative output rate must error")
	}
}

func makeConst(n int, v float64) []float64 {
	b := make([]float64, n)
	for i := range b {
		b[i] = v
	}
	return b
}
