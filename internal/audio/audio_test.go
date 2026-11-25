package audio

import (
	"math"
	"testing"

	"mediacheck/internal/fixture"
)

func sineBuffer(rate, channels, frames int, amplitude float64) *Buffer {
	b := &Buffer{Rate: rate, Channels: channels, Samples: make([]float64, frames*channels)}
	for i := 0; i < frames; i++ {
		v := amplitude * math.Sin(2*math.Pi*440*float64(i)/float64(rate))
		for c := 0; c < channels; c++ {
			b.Samples[i*channels+c] = v
		}
	}
	return b
}

func TestConvert441MonoTo48Stereo(t *testing.T) {
	in := sineBuffer(44100, 1, 4410, 0.2)
	out := ConvertFormat(in, TargetRate, TargetChannels)
	if out.Rate != 48000 || out.Channels != 2 {
		t.Fatalf("format=%d/%d", out.Rate, out.Channels)
	}
	wantFrames := int64(4410)*48000/44100 + 1
	if got := int64(out.Frames()); math.Abs(float64(got-wantFrames)) > 1 {
		t.Fatalf("frames=%d want~%d", got, wantFrames)
	}
	if out.Samples[0] != out.Samples[1] {
		t.Fatal("mono should duplicate both stereo channels")
	}
}

func TestNormalizeSineToTargetLUFSAndPeak(t *testing.T) {
	in := sineBuffer(TargetRate, 2, 2*TargetRate, 0.25)
	got := Normalize(in, -23, -1, 0)
	if math.Abs(got.LUFS+23) > 1 {
		t.Fatalf("lufs=%.3f", got.LUFS)
	}
	if got.TruePeakDBFS > -1 {
		t.Fatalf("true peak=%.3f", got.TruePeakDBFS)
	}
}

func TestSilenceHasDeterministicResult(t *testing.T) {
	in := &Buffer{Rate: TargetRate, Channels: 2, Samples: make([]float64, 4096)}
	first := Normalize(in, -23, -1, 0)
	second := Normalize(in, -23, -1, 0)
	if !first.Silent || !second.Silent {
		t.Fatal("silence not classified")
	}
	if first.LUFS != -120 || first.TruePeakDBFS != -120 {
		t.Fatalf("metrics lufs=%v peak=%v", first.LUFS, first.TruePeakDBFS)
	}
	if string(first.Buffer.ToS16()) != string(second.Buffer.ToS16()) {
		t.Fatal("silence encoding not deterministic")
	}
}

func TestShortMaterialIsNormalized(t *testing.T) {
	in := sineBuffer(TargetRate, 1, 480, 0.2) // 10 ms
	got := Normalize(in, -23, -1, 0)
	if !got.ShortMaterial {
		t.Fatal("short material not marked")
	}
	if math.Abs(got.LUFS+23) > 1 {
		t.Fatalf("short lufs=%.3f", got.LUFS)
	}
}

func TestSparseImpulsesRespectPeakCeiling(t *testing.T) {
	in := &Buffer{Rate: TargetRate, Channels: 1, Samples: make([]float64, TargetRate)}
	for i := 0; i < len(in.Samples); i += 4800 {
		in.Samples[i] = 1
	}
	got := Normalize(in, -23, -1, 0)
	if !got.PeakLimited {
		t.Fatal("expected peak limiter trim")
	}
	if got.TruePeakDBFS > -1 {
		t.Fatalf("true peak=%.3f > -1", got.TruePeakDBFS)
	}
}

func TestClippingFixtureIsCountedAndDeterministic(t *testing.T) {
	pcm := fixture.ClippingPCM(48000, 2, 4800)
	if n := CountClippedS16(pcm); n == 0 {
		t.Fatal("expected clipped samples")
	}
	b := FromS16(pcm, 48000, 2)
	first := Normalize(b, -23, -1, CountClippedS16(pcm)).Buffer.ToS16()
	second := Normalize(b, -23, -1, CountClippedS16(pcm)).Buffer.ToS16()
	if string(first) != string(second) {
		t.Fatal("clipping normalization not deterministic")
	}
	peak := TruePeakAmplitude(Normalize(b, -23, -1, 0).Buffer)
	if AmplitudeToDBFS(peak) > -1 {
		t.Fatalf("peak=%.3f dBFS", AmplitudeToDBFS(peak))
	}
}
