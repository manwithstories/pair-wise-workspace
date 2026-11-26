package audio

import (
	"math"
	"testing"

	"mediamix/internal/clock"
)

func toneF32(frames, ch int, hz, amp float64, rate int) []float32 {
	out := make([]float32, frames*ch)
	for i := 0; i < frames; i++ {
		v := amp * math.Sin(2*math.Pi*hz*float64(i)/float64(rate))
		for c := 0; c < ch; c++ {
			out[i*ch+c] = float32(v)
		}
	}
	return out
}

func TestLoudnessDeterministicAndPeakCapped(t *testing.T) {
	// Loud signal: sine at 0.9 amplitude, well above the -1 dBTP ceiling
	// (~0.891 linear). Must be attenuated and identical across two runs.
	const frames = 48000 // 1 second
	in := toneF32(frames, 2, 1000, 0.9, 48000)

	run := func() []float32 {
		l, err := NewLoudnessNormaliser(48000, 2)
		if err != nil {
			t.Fatalf("build normaliser: %v", err)
		}
		var out []float32
		// Feed in 20 ms frames like the real pipeline.
		for off := 0; off < len(in); off += 960 {
			end := off + 960
			if end > len(in) {
				end = len(in)
			}
			part, err := l.Process(in[off:end], int64(off/2))
			if err != nil {
				t.Fatalf("process: %v", err)
			}
			out = append(out, part...)
		}
		tail, err := l.Flush()
		if err != nil {
			t.Fatalf("flush: %v", err)
		}
		return append(out, tail...)
	}

	a, b := run(), run()
	if len(a) != len(b) {
		t.Fatalf("non-deterministic length: %d != %d", len(a), len(b))
	}
	for i := range a {
		if math.IsNaN(float64(a[i])) || math.IsNaN(float64(b[i])) {
			t.Fatalf("NaN at %d: a=%v b=%v (frame %d)", i, a[i], b[i], i/2)
		}
		if a[i] != b[i] {
			t.Fatalf("non-deterministic sample at %d: %v != %v", i, a[i], b[i])
		}
	}
	limit := math.Pow(10, MaxTruePeak/20) // ~0.8913
	var peak float64
	for _, v := range a {
		if x := math.Abs(float64(v)); x > peak {
			peak = x
		}
	}
	if peak > limit+1e-6 {
		t.Fatalf("true peak %.4f exceeds -1 dBTP limit %.4f", peak, limit)
	}
	if peak == 0 {
		t.Fatal("output was fully silenced")
	}
	if len(a) < frames*2 {
		t.Fatalf("output lost frames: got %d values, want >= %d", len(a), frames*2)
	}
}

func TestPLCRejectsTruncatedTail(t *testing.T) {
	p := NewPLC(DefaultPLCConfig())
	// No reference frame at all: concealment must fail honestly.
	if _, ok, err := p.Conceal(100, false); ok || !clock.IsCode(err, clock.ErrGap) {
		t.Fatalf("concealment without reference must fail, got ok=%v err=%v", ok, err)
	}
	p.Observe(toneF32(960, 2, 440, 0.5, 48000))
	// A truncated tail must never be reported as successful recovery.
	if _, ok, err := p.Conceal(200, true); ok || !clock.IsCode(err, clock.ErrTruncatedTail) {
		t.Fatalf("truncated tail must fail, got ok=%v err=%v", ok, err)
	}
	if p.TruncatedTails() != 1 {
		t.Fatalf("truncated tail counter = %d", p.TruncatedTails())
	}
}

func TestPLCDeterministicDecay(t *testing.T) {
	mk := func() []float32 {
		p := NewPLC(DefaultPLCConfig())
		p.Observe(toneF32(960, 2, 440, 0.5, 48000))
		out, ok, err := p.Conceal(480, false)
		if !ok || err != nil {
			t.Fatalf("conceal: %v", err)
		}
		return out
	}
	a, b := mk(), mk()
	if len(a) != len(b) || len(a) != 480*2 {
		t.Fatalf("concealed length mismatch: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("PLC not deterministic at %d", i)
		}
	}
	// Amplitude must decay monotonically in magnitude.
	prev := math.Abs(float64(a[0]))
	for f := 1; f < 480; f++ {
		cur := math.Abs(float64(a[f*2]))
		if cur > prev {
			t.Fatalf("concealed signal grew at frame %d", f)
		}
		prev = cur
	}
	if p := NewPLC(DefaultPLCConfig()); p.ConcealedFrames() != 0 {
		t.Fatal("fresh PLC should report zero concealed frames")
	}
}
