package loudness

import (
	"math"
	"testing"

	"avpipe/internal/media"
)

func tone(rate float64, frames, channels int, amp, freq float64) []float64 {
	out := make([]float64, frames*channels)
	for i := 0; i < frames; i++ {
		v := amp * math.Sin(2*math.Pi*freq*float64(i)/rate)
		for c := 0; c < channels; c++ {
			out[i*channels+c] = v
		}
	}
	return out
}

func run(t *testing.T, c *Controller, blockFrames int, samples []float64) []float64 {
	t.Helper()
	var out []float64
	for pos := 0; pos < len(samples); pos += blockFrames * c.channels {
		end := pos + blockFrames*c.channels
		if end > len(samples) {
			end = len(samples)
		}
		b, err := c.Update(media.Block{
			Track: "t", Rate: c.rate, Channels: c.channels, Samples: samples[pos:end],
		})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b.Samples...)
	}
	return out
}

func TestLoudnessTargetReached(t *testing.T) {
	rate := media.Rate{Num: 48000, Den: 1}
	c, err := New(Config{Rate: rate, Channels: 2})
	if err != nil {
		t.Fatal(err)
	}
	// A 0.1 amplitude tone is roughly -23 dBFS; the controller should lift it.
	samples := tone(48000, 48000*3, 2, 0.1, 440)
	out := run(t, c, 480, samples)
	// Measure the converged tail, skipping the first second of adaptation.
	tail := out[len(out)/2:]
	ms := MeanSquareDBFS(tail, 2)
	if ms < TargetDBFS-TargetTolerance || ms > TargetDBFS+TargetTolerance {
		t.Fatalf("loudness %.2f dBFS outside -20±1", ms)
	}
	if peak := PeakDBFS(out); peak > PeakCeilingDBFS {
		t.Fatalf("peak %.2f dBFS above ceiling", peak)
	}
}

func TestPeakCeiling(t *testing.T) {
	rate := media.Rate{Num: 48000, Den: 1}
	c, _ := New(Config{Rate: rate, Channels: 1})
	// A near-1.0 signal at exactly the ceiling is the classic limiter case.
	samples := tone(48000, 48000, 1, 1.5, 100)
	out := run(t, c, 256, samples)
	if peak := PeakDBFS(out); peak > PeakCeilingDBFS+1e-9 {
		t.Fatalf("peak %.3f dBFS, ceiling %.1f", peak, PeakCeilingDBFS)
	}
}

func TestFirstBlockMetadata(t *testing.T) {
	rate := media.Rate{Num: 48000, Den: 1}
	c, _ := New(Config{Rate: rate, Channels: 2})
	b := media.Block{Track: "mix", Seq: 9, Start: 1234, Rate: rate, Channels: 2, Samples: make([]float64, 20)}
	out, err := c.Update(b)
	if err != nil {
		t.Fatal(err)
	}
	if out.Seq != 9 || out.Start != 1234 || out.Track != "mix" || out.Rate != rate {
		t.Fatalf("metadata changed: %+v", out)
	}
}

func TestWindowIs200Millis(t *testing.T) {
	rate := media.Rate{Num: 48000, Den: 1}
	c, _ := New(Config{Rate: rate, Channels: 1})
	if c.window != 9600 {
		t.Fatalf("window=%d want 9600", c.window)
	}
	if c.Stats().WindowMS != 200 {
		t.Fatalf("stats=%+v", c.Stats())
	}
}

func TestInvalidBlockRejected(t *testing.T) {
	rate := media.Rate{Num: 48000, Den: 1}
	c, _ := New(Config{Rate: rate, Channels: 2})
	if _, err := c.Update(media.Block{Track: "t", Rate: rate, Channels: 1, Samples: []float64{1}}); err == nil {
		t.Fatal("channel mismatch accepted")
	}
	if _, err := c.Update(media.Block{Track: "t", Rate: media.Rate{Num: 44100, Den: 1}, Channels: 2, Samples: []float64{1, 1}}); err == nil {
		t.Fatal("rate mismatch accepted")
	}
	if _, err := c.Update(media.Block{Track: "t", Rate: rate, Channels: 2, Samples: []float64{1}}); err == nil {
		t.Fatal("misaligned samples accepted")
	}
	if _, err := New(Config{Rate: rate}); err == nil {
		t.Fatal("zero channels accepted")
	}
	if _, err := New(Config{Channels: 1}); err == nil {
		t.Fatal("invalid rate accepted")
	}
}

func TestSilenceStable(t *testing.T) {
	rate := media.Rate{Num: 48000, Den: 1}
	c, _ := New(Config{Rate: rate, Channels: 1})
	out := run(t, c, 480, make([]float64, 48000))
	for _, s := range out {
		if s != 0 {
			t.Fatalf("silence produced %v", s)
		}
	}
	if math.IsNaN(c.Finish()) {
		t.Fatal("gain became NaN")
	}
}

func TestTargetOverride(t *testing.T) {
	rate := media.Rate{Num: 48000, Den: 1}
	c, err := New(Config{Rate: rate, Channels: 1, UseCustomTarget: true, TargetDBFS: -12})
	if err != nil {
		t.Fatal(err)
	}
	samples := tone(48000, 48000*2, 1, 0.05, 500)
	out := run(t, c, 512, samples)
	ms := MeanSquareDBFS(out[len(out)/2:], 1)
	if math.Abs(ms-(-12)) > 1 {
		t.Fatalf("custom target loudness %.2f", ms)
	}
}
