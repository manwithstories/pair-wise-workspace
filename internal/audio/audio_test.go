package audio_test

import (
	"math"
	"testing"

	"github.com/fieldcut/fieldcut/internal/audio"
	"github.com/fieldcut/fieldcut/internal/fixture"
)

func TestResampleTo48k(t *testing.T) {
	t.Parallel()
	rs := audio.NewResampler(2)
	// One second of 44.1 kHz stereo tone.
	src := fixture.GeneratePCM(fixture.PCMParams{
		SampleRate: 44100, Layout: audio.LayoutStereo, Seconds: 1, FreqHz: 440, Seed: 1,
	})
	out, err := rs.Process(src.Samples, 44100, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := 48000 * 2
	// Linear conversion tolerance of a few samples at block boundaries.
	if math.Abs(float64(len(out)-want)) > 8 {
		t.Fatalf("resampled length %d, want ~%d", len(out), want)
	}
}

func TestResamplePassthroughAt48k(t *testing.T) {
	t.Parallel()
	rs := audio.NewResampler(1)
	in := make([]float32, 480)
	for i := range in {
		in[i] = 0.5
	}
	out, err := rs.Process(in, 48000, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(in) {
		t.Fatalf("48k passthrough changed length: %d != %d", len(out), len(in))
	}
}

func TestDownmix51ExplicitMapping(t *testing.T) {
	t.Parallel()
	// L=1, R=1, C=1, LFE=1, surrounds=1: verify documented coefficients.
	f := &audio.Frame{
		SampleRate: 48000, Layout: audio.Layout51,
		Samples: []float32{1, 1, 1, 1, 1, 1},
	}
	stereo, lfePeak, err := audio.Downmix(f)
	if err != nil {
		t.Fatal(err)
	}
	// ITU-R BS.775 Annex 1: center and surround each at -3 dB (1/sqrt(2)).
	gain := 0.70710678
	want := 1.0 + gain + gain
	if math.Abs(float64(stereo[0])-want) > 1e-6 {
		t.Fatalf("left mix %f != %f", stereo[0], want)
	}
	if lfePeak != 1.0 {
		t.Fatalf("LFE peak must be tracked separately, got %f", lfePeak)
	}
}

func TestMonoAndStereoDownmix(t *testing.T) {
	t.Parallel()
	mono := &audio.Frame{SampleRate: 48000, Layout: audio.LayoutMono, Samples: []float32{0.7}}
	out, _, err := audio.Downmix(mono)
	if err != nil || out[0] != 0.7 || out[1] != 0.7 {
		t.Fatalf("mono dual-mono mapping wrong: %v err=%v", out, err)
	}

	st := &audio.Frame{SampleRate: 48000, Layout: audio.LayoutStereo, Samples: []float32{0.3, 0.6}}
	out2, _, err := audio.Downmix(st)
	if err != nil || out2[0] != 0.3 || out2[1] != 0.6 {
		t.Fatalf("stereo passthrough wrong: %v err=%v", out2, err)
	}
}

func TestNormalizeHitsTargetLUFSAndPeak(t *testing.T) {
	t.Parallel()
	frames := fixture.GeneratePCMSuite(21, 4.0)
	stereo, _, stats, err := audio.NormalizePipeline(frames)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(stats.OutputLUFS-audio.TargetLUFS) > 0.2 {
		t.Fatalf("output loudness %f not within 0.2 of %f", stats.OutputLUFS, audio.TargetLUFS)
	}
	if stats.TruePeakDB > audio.TruePeakLimitDB+0.05 {
		t.Fatalf("true peak %f exceeds -1 dBTP", stats.TruePeakDB)
	}
	// No sample may exceed the linear ceiling.
	ceiling := math.Pow(10, audio.TruePeakLimitDB/20)
	for i, x := range stereo {
		if math.Abs(float64(x)) > ceiling+1e-7 {
			t.Fatalf("sample %d over ceiling: %f", i, x)
		}
	}
}

func TestSilentProgramGetsNoGain(t *testing.T) {
	t.Parallel()
	silent := make([]float32, 48000*2)
	out, stats := audio.Normalize(silent, 48000)
	if stats.GainDB != 0 {
		t.Fatalf("silent program must not invent gain, got %f dB", stats.GainDB)
	}
	if stats.SilentBlocks == 0 {
		t.Fatal("silent blocks must be recorded")
	}
	for i, x := range out {
		if x != 0 {
			t.Fatalf("silent output changed at %d: %f", i, x)
		}
	}
}

func TestInvalidLayoutRejected(t *testing.T) {
	t.Parallel()
	bad := &audio.Frame{SampleRate: 48000, Layout: "7.1", Samples: []float32{0, 0}}
	if _, _, err := audio.Downmix(bad); err == nil {
		t.Fatal("unknown channel layout must be rejected")
	}
}
