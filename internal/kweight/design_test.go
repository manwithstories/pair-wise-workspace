package kweight

import (
	"errors"
	"math"
	"testing"
)

func TestStandard48kCoefficients(t *testing.T) {
	d, err := ForRate(48000)
	if err != nil {
		t.Fatal(err)
	}
	shelfB := []float64{1.53512485958697, -2.69169618940638, 1.19839281085285}
	shelfA := []float64{-1.69065929318241, 0.73248077421585}
	hpA := []float64{-1.99004745483398, 0.99007225036621}
	gotB := []float64{d.Shelf.B0, d.Shelf.B1, d.Shelf.B2}
	gotA := []float64{d.Shelf.A1, d.Shelf.A2}
	for i := range shelfB {
		if math.Abs(gotB[i]-shelfB[i]) > 1e-12 {
			t.Fatalf("shelf b%d = %.15g", i, gotB[i])
		}
	}
	for i := range shelfA {
		if math.Abs(gotA[i]-shelfA[i]) > 1e-12 {
			t.Fatalf("shelf a%d = %.15g", i+1, gotA[i])
		}
	}
	if d.HighPass.B0 != 1 || d.HighPass.B1 != -2 || d.HighPass.B2 != 1 {
		t.Fatalf("high-pass numerator = %+v", d.HighPass)
	}
	if math.Abs(d.HighPass.A1-hpA[0]) > 1e-12 || math.Abs(d.HighPass.A2-hpA[1]) > 1e-12 {
		t.Fatalf("high-pass denominator = %+v", d.HighPass)
	}
	if got := 20 * math.Log10(d.MagnitudeAt(48000, 1000)); math.Abs(got-0.6977) > 0.001 {
		t.Fatalf("1 kHz gain = %.4f dB", got)
	}
}

func TestAllSupportedRatesHaveStablePoles(t *testing.T) {
	for _, fs := range []int{32000, 32001, 44100, 48000, 88200, 96000, 176400, 192000} {
		d, err := ForRate(fs)
		if err != nil {
			t.Fatalf("%d Hz: %v", fs, err)
		}
		if err := d.Validate(); err != nil {
			t.Fatalf("%d Hz: %v", fs, err)
		}
	}
	for _, fs := range []int{0, 31999, 192001} {
		if _, err := ForRate(fs); !errors.Is(err, ErrUnsupportedSampleRate) {
			t.Fatalf("%d Hz error = %v", fs, err)
		}
	}
}

func TestChunkedFilterMatchesSinglePass(t *testing.T) {
	d, err := ForRate(48000)
	if err != nil {
		t.Fatal(err)
	}
	all, _ := NewBank(d, 2)
	chunked, _ := NewBank(d, 2)
	const frames = 20000
	src := make([]float64, frames*2)
	for i := 0; i < frames; i++ {
		src[i*2] = 0.25 * math.Sin(2*math.Pi*997*float64(i)/48000)
		src[i*2+1] = 0.1 * math.Sin(2*math.Pi*3000*float64(i)/48000)
	}
	want := make([]float64, len(src))
	got := make([]float64, len(src))
	if err := all.Process(want, src, frames); err != nil {
		t.Fatal(err)
	}
	for start := 0; start < frames; start += 777 {
		end := start + 777
		if end > frames {
			end = frames
		}
		if err := chunked.Process(got[start*2:end*2], src[start*2:end*2], end-start); err != nil {
			t.Fatal(err)
		}
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample %d = %.15g, want %.15g", i, got[i], want[i])
		}
	}
}

func TestChannelsKeepIndependentState(t *testing.T) {
	d, _ := ForRate(48000)
	bank, _ := NewBank(d, 2)
	src := make([]float64, 1000*2)
	src[0] = 1
	dst := make([]float64, len(src))
	if err := bank.Process(dst, src, 1000); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(dst); i += 2 {
		if dst[i] != 0 {
			t.Fatalf("right channel sample %d = %g", i/2, dst[i])
		}
	}
}
