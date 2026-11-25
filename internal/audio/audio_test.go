package audio_test

import (
	"math"
	"testing"

	"mediad/internal/audio"
)

func TestResampleAndFixedStereoLayout(t *testing.T) {
	inFrames := 4410
	samples := make([]float64, inFrames*6)
	for f := 0; f < inFrames; f++ {
		for c := 0; c < 6; c++ {
			samples[f*6+c] = 0.2 * math.Sin(2*math.Pi*440*float64(f)/44100+float64(c))
		}
	}
	in := audio.Buffer{SampleRate: 44100, Channels: 6, Samples: samples}
	down, err := audio.Downmix(in, "stereo")
	if err != nil {
		t.Fatal(err)
	}
	if down.Channels != 2 || down.FrameCount() != inFrames {
		t.Fatalf("downmix format sr=%d ch=%d frames=%d", down.SampleRate, down.Channels, down.FrameCount())
	}
	out, err := audio.Resample(down, 48000)
	if err != nil {
		t.Fatal(err)
	}
	wantFrames := int(math.Round(float64(inFrames) * 48000 / 44100))
	if out.SampleRate != 48000 || out.Channels != 2 || out.FrameCount() != wantFrames {
		t.Fatalf("resample sr=%d ch=%d frames=%d want %d", out.SampleRate, out.Channels, out.FrameCount(), wantFrames)
	}
	if _, err := audio.Downmix(in, "surround"); err == nil {
		t.Fatal("unknown layout should fail")
	}
}

func TestMasterLoudnessAndTruePeak(t *testing.T) {
	rate := 48000
	frames := 3 * rate
	samples := make([]float64, frames*2)
	for f := 0; f < frames; f++ {
		v := 0.22 * math.Sin(2*math.Pi*260*float64(f)/float64(rate))
		samples[f*2] = v
		samples[f*2+1] = v
	}
	in := audio.Buffer{SampleRate: rate, Channels: 2, Samples: samples}
	mastered, report, err := audio.Master(in, -18, -1)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(report.IntegratedLUFS+18) > 0.5 {
		t.Fatalf("integrated %.2f not within 0.5 of -18", report.IntegratedLUFS)
	}
	if report.TruePeakDBTP > -1 {
		t.Fatalf("true peak %.3f exceeds -1 dBTP", report.TruePeakDBTP)
	}
	wav, err := audio.EncodeWAV(mastered)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := audio.DecodeWAV(wav)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.SampleRate != 48000 || decoded.Channels != 2 {
		t.Fatalf("decoded format %d/%d", decoded.SampleRate, decoded.Channels)
	}
	after := audio.Measure(decoded)
	if math.Abs(after.IntegratedLUFS+18) > 0.5 {
		t.Fatalf("decoded LUFS %.2f outside tolerance", after.IntegratedLUFS)
	}
	if after.TruePeakDBTP > -1+1e-9 {
		t.Fatalf("decoded true peak %.3f too high", after.TruePeakDBTP)
	}
}

func TestUnsupportedMatrixChannelCount(t *testing.T) {
	in := audio.Buffer{SampleRate: 48000, Channels: 3, Samples: make([]float64, 30)}
	if _, err := audio.Downmix(in, "stereo"); err == nil {
		t.Fatal("3-channel source has no fixed matrix and must not be guessed")
	}
}
