package codec

import (
	"math"
	"testing"

	"rcctl/internal/source"
)

func TestTransformRoundTrip(t *testing.T) {
	var block [64]float64
	for i := range block {
		block[i] = float64((i*17)%255) - 128
	}
	got := Inverse(ptr(Forward(&block)))
	for i := range block {
		if math.Abs(got[i]-block[i]) > 1e-9 {
			t.Fatalf("coefficient %d: got %f want %f", i, got[i], block[i])
		}
	}
}

func TestQuantizationRoundTrip(t *testing.T) {
	var block [64]float64
	for i := range block {
		block[i] = float64(i*i%31) - 10
	}
	coeff := Forward(&block)
	q := Quantize(&coeff, 12)
	restored := Inverse(ptr(Dequantize(&q, 12)))
	for i := range restored {
		if math.IsNaN(restored[i]) || math.IsInf(restored[i], 0) {
			t.Fatalf("non-finite inverse sample %d", i)
		}
	}
}

func TestHigherQuantizerUsesFewerBytes(t *testing.T) {
	synth, err := source.New("unit", 2, 32, 32, 7)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := NewEncoder(32, 32)
	if err != nil {
		t.Fatal(err)
	}
	f0, _ := synth.At(0)
	f1, _ := synth.At(1)
	low, err := enc.EncodeFrame(f0, nil, 8, true)
	if err != nil {
		t.Fatal(err)
	}
	high, err := enc.EncodeFrame(f1, &f0, 40, false)
	if err != nil {
		t.Fatal(err)
	}
	if high.Bytes >= low.Bytes {
		t.Fatalf("q=40 frame (%d bytes) not smaller than q=8 intra frame (%d bytes)", high.Bytes, low.Bytes)
	}
}
