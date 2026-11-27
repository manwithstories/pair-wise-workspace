package resample

import (
	"math"
	"testing"

	"avpipe/internal/media"
)

func sine(rate float64, frames, channels int, freq float64) []float64 {
	out := make([]float64, frames*channels)
	for i := 0; i < frames; i++ {
		v := 0.4 * math.Sin(2*math.Pi*freq*float64(i)/rate)
		for c := 0; c < channels; c++ {
			out[i*channels+c] = v
		}
	}
	return out
}

func runAll(t *testing.T, r *Resampler, rate media.Rate, samples []float64, blockFrames, channels int) []media.Block {
	t.Helper()
	var out []media.Block
	start := int64(0)
	for pos := 0; pos < len(samples); pos += blockFrames * channels {
		end := pos + blockFrames*channels
		if end > len(samples) {
			end = len(samples)
		}
		blocks, err := r.Process(media.Block{
			Track: "t", Start: start, Rate: rate, Channels: channels,
			Samples: samples[pos:end],
		})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, blocks...)
		start += int64((end - pos) / channels)
	}
	blocks, err := r.Flush()
	if err != nil {
		t.Fatal(err)
	}
	return append(out, blocks...)
}

func checkContiguous(t *testing.T, blocks []media.Block, rate media.Rate, channels int) int64 {
	t.Helper()
	var next int64
	var seq uint64
	for i, b := range blocks {
		if b.Start != next {
			t.Fatalf("block %d starts at %d, expected %d", i, b.Start, next)
		}
		if b.Rate != rate || b.Channels != channels {
			t.Fatalf("block %d format %+v", i, b)
		}
		if b.Frames() > 4096 {
			t.Fatalf("block %d has %d frames", i, b.Frames())
		}
		if i > 0 && b.Seq != seq+1 {
			t.Fatalf("block %d seq %d", i, b.Seq)
		}
		seq = b.Seq
		next += int64(b.Frames())
	}
	return next
}

func TestIdentityRateRoundTrip(t *testing.T) {
	rate := media.Rate{Num: 48000, Den: 1}
	r, err := New(Config{Src: rate, Dst: rate})
	if err != nil {
		t.Fatal(err)
	}
	samples := sine(48000, 3000, 2, 1000)
	blocks := runAll(t, r, rate, samples, 441, 2)
	total := checkContiguous(t, blocks, rate, 2)
	if total != 3000 {
		t.Fatalf("total out=%d want 3000", total)
	}
	// Sum of emitted frames must equal the exact rational conversion.
	clock, _ := media.NewClock(rate)
	want, _ := clock.Convert(3000, rate)
	if total != want {
		t.Fatalf("total=%d exact=%d", total, want)
	}
}

func TestDownsampleNoDrift(t *testing.T) {
	src := media.Rate{Num: 44100, Den: 1}
	dst := media.Rate{Num: 48000, Den: 1}
	r, err := New(Config{Src: src, Dst: dst})
	if err != nil {
		t.Fatal(err)
	}
	samples := sine(44100, 44100, 1, 440)
	blocks := runAll(t, r, src, samples, 441, 1)
	total := checkContiguous(t, blocks, dst, 1)
	clock, _ := media.NewClock(src)
	want, _ := clock.Convert(44100, dst)
	if total != want {
		t.Fatalf("total=%d want exact %d", total, want)
	}
}

func Test48kTo16kExactLength(t *testing.T) {
	src := media.Rate{Num: 48000, Den: 1}
	dst := media.Rate{Num: 16000, Den: 1}
	r, _ := New(Config{Src: src, Dst: dst, MaxBlockFrames: 100})
	samples := sine(48000, 4800, 1, 500)
	blocks := runAll(t, r, src, samples, 333, 1)
	total := checkContiguous(t, blocks, dst, 1)
	if total != 1600 {
		t.Fatalf("total=%d want 1600", total)
	}
}

func TestBlockBoundaryIndependence(t *testing.T) {
	// The same input split differently must yield bit-identical output.
	src := media.Rate{Num: 44100, Den: 1}
	dst := media.Rate{Num: 48000, Den: 1}
	samples := sine(44100, 8820, 2, 700)

	build := func(blockFrames int) []float64 {
		r, err := New(Config{Src: src, Dst: dst})
		if err != nil {
			t.Fatal(err)
		}
		blocks := runAll(t, r, src, samples, blockFrames, 2)
		var flat []float64
		for _, b := range blocks {
			flat = append(flat, b.Samples...)
		}
		return flat
	}
	a := build(441)
	b := build(1000)
	if len(a) != len(b) {
		t.Fatalf("lengths differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("sample %d differs: %v vs %v", i, a[i], b[i])
		}
	}
}

func TestPassbandPreserved(t *testing.T) {
	src := media.Rate{Num: 48000, Den: 1}
	dst := media.Rate{Num: 24000, Den: 1}
	r, _ := New(Config{Src: src, Dst: dst})
	samples := sine(48000, 48000, 1, 1000)
	blocks := runAll(t, r, src, samples, 1024, 1)
	var flat []float64
	for _, b := range blocks {
		flat = append(flat, b.Samples...)
	}
	// Skip the edges where the truncated kernel rings.
	var peak, rms float64
	n := 0
	for i := 100; i < len(flat)-100; i++ {
		rms += flat[i] * flat[i]
		if math.Abs(flat[i]) > peak {
			peak = math.Abs(flat[i])
		}
		n++
	}
	rms = math.Sqrt(rms / float64(n))
	if math.Abs(peak-0.4) > 0.02 {
		t.Fatalf("peak=%f want ~0.4", peak)
	}
	if math.Abs(rms-0.4/math.Sqrt2) > 0.02 {
		t.Fatalf("rms=%f want ~%f", rms, 0.4/math.Sqrt2)
	}
}

func TestStopbandRejected(t *testing.T) {
	src := media.Rate{Num: 48000, Den: 1}
	dst := media.Rate{Num: 16000, Den: 1}
	r, _ := New(Config{Src: src, Dst: dst})
	// 16 kHz is well above the 8 kHz output Nyquist and outside the
	// transition band a 32-tap windowed sinc can provide.
	samples := sine(48000, 48000, 1, 16000)
	blocks := runAll(t, r, src, samples, 512, 1)
	var peak float64
	var count int
	for _, b := range blocks {
		for _, s := range b.Samples {
			if math.Abs(s) > peak {
				peak = math.Abs(s)
			}
			count++
		}
	}
	if peak > 0.05 {
		t.Fatalf("stopband leak peak=%f", peak)
	}
	if count != 16000 {
		t.Fatalf("count=%d", count)
	}
}

func TestFlushIdempotentAndTerminal(t *testing.T) {
	rate := media.Rate{Num: 48000, Den: 1}
	r, _ := New(Config{Src: rate, Dst: rate})
	if _, err := r.Process(media.Block{Track: "t", Rate: rate, Channels: 1, Samples: make([]float64, 10)}); err != nil {
		t.Fatal(err)
	}
	first, err := r.Flush()
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Flush()
	if err != nil || len(second) != 0 {
		t.Fatalf("second flush=%v err=%v", second, err)
	}
	if len(first) == 0 {
		t.Fatal("first flush produced nothing")
	}
	if _, err := r.Process(media.Block{Track: "t", Rate: rate, Channels: 1, Samples: make([]float64, 1)}); err == nil {
		t.Fatal("process after flush accepted")
	}
}

func TestInvalidInputs(t *testing.T) {
	if _, err := New(Config{Src: media.Rate{}, Dst: media.Rate{Num: 48000, Den: 1}}); err == nil {
		t.Fatal("invalid src accepted")
	}
	rate := media.Rate{Num: 48000, Den: 1}
	r, _ := New(Config{Src: rate, Dst: media.Rate{Num: 16000, Den: 1}})
	if _, err := r.Process(media.Block{Track: "t", Rate: media.Rate{Num: 44100, Den: 1}, Channels: 1, Samples: []float64{0}}); err == nil {
		t.Fatal("wrong rate accepted")
	}
	r2, _ := New(Config{Src: rate, Dst: media.Rate{Num: 16000, Den: 1}})
	if _, err := r2.Process(media.Block{Track: "t", Rate: rate, Channels: 2, Start: 0, Samples: []float64{0, 0}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r2.Process(media.Block{Track: "t", Rate: rate, Channels: 1, Start: 1, Samples: []float64{0}}); err == nil {
		t.Fatal("channel change accepted")
	}
	r3, _ := New(Config{Src: rate, Dst: media.Rate{Num: 16000, Den: 1}})
	if _, err := r3.Process(media.Block{Track: "t", Rate: rate, Channels: 1, Start: 5, Samples: []float64{0}}); err == nil {
		t.Fatal("non-contiguous start accepted")
	}
}

func TestEmptyFlush(t *testing.T) {
	rate := media.Rate{Num: 48000, Den: 1}
	r, _ := New(Config{Src: rate, Dst: rate})
	out, err := r.Flush()
	if err != nil || len(out) != 0 {
		t.Fatalf("out=%v err=%v", out, err)
	}
}
