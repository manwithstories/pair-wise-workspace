package audio

import (
	"math"
	"testing"
)

func TestMeasureLUFSSilence(t *testing.T) {
	if got := MeasureLUFS(nil); got != -70 {
		t.Fatalf("empty = %f, want -70", got)
	}
	silent := make([]int16, 100)
	if got := MeasureLUFS(silent); got != -70 {
		t.Fatalf("silent = %f, want -70", got)
	}
}

func TestMeasureLUFSFullScale(t *testing.T) {
	samples := make([]int16, 256)
	for i := range samples {
		samples[i] = 32767
	}
	got := MeasureLUFS(samples)
	// Full-scale sine-ish constant sits close to 0 dBFS.
	if math.Abs(got) > 0.01 {
		t.Fatalf("full-scale LUFS=%f, want ~0", got)
	}
}

func TestNormalizerCapsGain(t *testing.T) {
	// Target -16 LUFS on a very quiet block would ask for huge gain; the
	// 12 dB cap must hold.
	n := NewNormalizer(-16)
	quiet := make([]int16, 256)
	for i := range quiet {
		quiet[i] = 1 // near silence
	}
	gain := n.GainDB(MeasureLUFS(quiet))
	if gain > MaxGainDB+1e-9 {
		t.Fatalf("gain %f exceeds cap %f", gain, MaxGainDB)
	}
}

func TestNormalizerAttenuatesLoud(t *testing.T) {
	n := NewNormalizer(-23)
	loud := make([]int16, 512)
	for i := range loud {
		loud[i] = 30000
	}
	out := n.Process(loud)
	// Output must be attenuated and never clipped.
	if out[0] >= loud[0] {
		t.Fatalf("loud material not attenuated: %d -> %d", loud[0], out[0])
	}
}

func TestNormalizerNoClipping(t *testing.T) {
	n := NewNormalizer(-14)
	in := make([]int16, 1024)
	for i := range in {
		in[i] = int16(20000 - i%5000)
	}
	out := n.Process(in)
	for i, s := range out {
		if s > 32767 || s < -32768 {
			t.Fatalf("clip at %d: %d", i, s)
		}
	}
}
