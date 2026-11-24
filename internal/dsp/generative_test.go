package dsp_test

import (
	"math"
	"math/rand"
	"testing"

	"mediad/internal/dsp"
	"mediad/internal/pcm"
)

func randomAudio(t *testing.T, seed int64, rate, channels, frames int) *pcm.Audio {
	t.Helper()
	a, err := pcm.NewAudio(rate, channels, frames)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(seed))
	for i := range a.Samples {
		a.Samples[i] = rng.Float64()*2 - 1
	}
	return a
}

func rms(a *pcm.Audio) float64 {
	var sum float64
	for _, v := range a.Samples {
		sum += v * v
	}
	return math.Sqrt(sum / float64(len(a.Samples)))
}

// TestGenerativeResampleLayoutLoudness exercises many random buffers and
// checks canonical format, matrix bounds, peak limiting and no upward gain.
func TestGenerativeResampleLayoutLoudness(t *testing.T) {
	for seed := int64(1); seed <= 40; seed++ {
		seconds := 1 + int(seed%4)
		in, err := pcm.NewAudio(44100, 6, seconds*44100)
		if err != nil {
			t.Fatal(err)
		}
		rng := rand.New(rand.NewSource(seed))
		for i := range in.Samples {
			// Include exact zero runs to model packet loss/silence.
			if i%997 < 20 {
				in.Samples[i] = 0
			} else {
				in.Samples[i] = rng.Float64()*1.6 - 0.8
			}
		}

		stereo, err := dsp.NormalizeLayout(in)
		if err != nil {
			t.Fatalf("seed %d layout: %v", seed, err)
		}
		if stereo.Channels != 2 || stereo.Frames != in.Frames {
			t.Fatalf("seed %d bad layout %+v", seed, stereo)
		}
		for _, v := range stereo.Samples {
			if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > 1.000001 {
				t.Fatalf("seed %d invalid/overflow sample %v", seed, v)
			}
		}

		out, err := dsp.Resample(stereo, 48000)
		if err != nil {
			t.Fatalf("seed %d resample: %v", seed, err)
		}
		wantFrames := seconds * 48000
		if out.SampleRate != 48000 || out.Channels != 2 || out.Frames != wantFrames {
			t.Fatalf("seed %d format rate=%d ch=%d frames=%d want=%d", seed, out.SampleRate, out.Channels, out.Frames, wantFrames)
		}

		limited, report, err := dsp.ProcessLoudness(out)
		if err != nil {
			t.Fatalf("seed %d loudness: %v", seed, err)
		}
		if report.Gain > 1.000001 {
			t.Fatalf("seed %d applied upward gain %f", seed, report.Gain)
		}
		if got := dsp.TruePeak(limited); got > math.Pow(10, dsp.CeilingDBTP/20)+1e-9 {
			t.Fatalf("seed %d true peak %f exceeds ceiling", seed, got)
		}
		if rms(limited) > rms(out)+1e-12 {
			t.Fatalf("seed %d RMS increased: %f > %f", seed, rms(limited), rms(out))
		}
	}
}

func TestLayoutRelations(t *testing.T) {
	mono, _ := pcm.NewAudio(48000, 1, 3)
	for i := range mono.Samples {
		mono.Samples[i] = 0.25
	}
	s, err := dsp.NormalizeLayout(mono)
	if err != nil {
		t.Fatal(err)
	}
	for f := 0; f < 3; f++ {
		if s.Samples[f*2] != s.Samples[f*2+1] || s.Samples[f*2] != 0.25 {
			t.Fatalf("mono matrix mismatch: %v", s.Samples)
		}
	}

	st := randomAudio(t, 7, 48000, 2, 100)
	id, err := dsp.NormalizeLayout(st)
	if err != nil {
		t.Fatal(err)
	}
	for i := range st.Samples {
		if id.Samples[i] != st.Samples[i] {
			t.Fatal("stereo identity matrix changed samples")
		}
	}
}

func TestSilenceIsNotAmplifiedOrNoised(t *testing.T) {
	a, _ := pcm.NewAudio(48000, 2, 48000)
	out, report, err := dsp.ProcessLoudness(a)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range out.Samples {
		if v != 0 {
			t.Fatalf("silence became %v", v)
		}
	}
	if report.IntegratedLUFS > dsp.SilenceLUFS+1 {
		t.Fatalf("silence loudness %f", report.IntegratedLUFS)
	}

	low := randomAudio(t, 9, 48000, 2, 48000)
	for i := range low.Samples {
		low.Samples[i] *= 1e-6
	}
	before := rms(low)
	after, rep, _ := dsp.ProcessLoudness(low)
	if rep.Gain > 1 {
		t.Fatal("low-level material was boosted")
	}
	if rms(after) > before+1e-15 {
		t.Fatal("noise floor increased")
	}
}

func TestResampleDeterministicAndSameRate(t *testing.T) {
	a := randomAudio(t, 11, 48000, 2, 9600)
	b, err := dsp.Resample(a, 48000)
	if err != nil {
		t.Fatal(err)
	}
	if &b.Samples[0] == &a.Samples[0] {
		t.Fatal("same-rate resample should clone, not alias")
	}
	for i := range a.Samples {
		if b.Samples[i] != a.Samples[i] {
			t.Fatal("same-rate audio changed")
		}
	}
}
