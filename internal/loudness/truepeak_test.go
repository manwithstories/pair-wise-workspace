package loudness

import (
	"math"
	"testing"
)

func TestQuarterRatePhaseSineTruePeak(t *testing.T) {
	const fs = 48000
	tp, err := NewTruePeak(1)
	if err != nil {
		t.Fatal(err)
	}
	const frames = fs * 2
	samples := make([]float64, frames)
	amp := math.Sqrt2
	for i := range samples {
		samples[i] = amp * math.Sin(math.Pi*float64(i)/2+math.Pi/4)
	}
	if err := tp.Add(samples, frames); err != nil {
		t.Fatal(err)
	}
	got := tp.DBTP()
	want := 3.0103
	if math.Abs(got-want) > 0.15 {
		t.Fatalf("true peak %.4f dBTP, want %.4f +/- 0.15", got, want)
	}
}

func TestTruePeakDoesNotMissLastSample(t *testing.T) {
	tp, err := NewTruePeak(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := tp.Add([]float64{0, 0.25, 0.5}, 3); err != nil {
		t.Fatal(err)
	}
	if got := tp.Peak(); got != 0.5 {
		t.Fatalf("peak = %g, want 0.5", got)
	}
}

func TestOneKilohertzChunkedTruePeak(t *testing.T) {
	tp, err := NewTruePeak(1)
	if err != nil {
		t.Fatal(err)
	}
	const fs = 48000
	samples := make([]float64, fs)
	for i := range samples {
		samples[i] = 0.1 * math.Sin(2*math.Pi*1000*float64(i)/fs)
	}
	for start := 0; start < len(samples); start += 8192 {
		end := start + 8192
		if end > len(samples) {
			end = len(samples)
		}
		if err := tp.Add(samples[start:end], end-start); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("raw peak=%g dBTP=%.4f", tp.Peak(), tp.DBTP())
	if got := tp.DBTP(); math.Abs(got-(-20)) > 0.1 {
		t.Fatalf("1 kHz true peak %.4f, want -20", got)
	}
}
