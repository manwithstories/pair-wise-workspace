package audio

import (
	"math"
	"testing"
)

func TestLayoutsAndExplicitCoefficients(t *testing.T) {
	cases := map[Layout]int{
		LayoutMono: 1, LayoutStereo: 2, LayoutFiveOne: 6, LayoutSevenOne: 8,
	}
	for layout, want := range cases {
		if got := layout.Channels(); got != want {
			t.Fatalf("%s channels=%d want%d", layout, got, want)
		}
		if _, err := ParseLayout(string(layout)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ParseLayout("9.1"); err == nil {
		t.Fatal("unknown layout accepted")
	}
	if math.Abs(CenterMixCoefficient-1/math.Sqrt2) > 1e-12 {
		t.Fatalf("center coefficient not explicit -3dB: %v", CenterMixCoefficient)
	}
	if LFEMixCoefficient != 0.5 {
		t.Fatalf("LFE coefficient=%v want0.5", LFEMixCoefficient)
	}
}

func TestDownmixFiveOneUsesAllChannels(t *testing.T) {
	in := NewAudio(48000, 6, 1)
	for c := range in.Channels {
		in.Channels[c][0] = 0.2
	}
	out, err := Downmix(in, LayoutFiveOne)
	if err != nil {
		t.Fatal(err)
	}
	wantL := 0.2 + 0.2*CenterMixCoefficient + 0.2*LFEMixCoefficient + 0.2*SurroundMixCoefficient
	if math.Abs(out.Channels[0][0]-wantL) > 1e-12 {
		t.Fatalf("L downmix=%v want%v", out.Channels[0][0], wantL)
	}
}

func sineAudio(rate, frames, channels int, amp float64) Audio {
	a := NewAudio(rate, channels, frames)
	for c := 0; c < channels; c++ {
		for i := 0; i < frames; i++ {
			a.Channels[c][i] = amp * math.Sin(2*math.Pi*440*float64(i)/float64(rate))
		}
	}
	a.Layout = LayoutStereo
	return a
}

func TestPolyphaseRationalDurationAndPositions(t *testing.T) {
	fir, err := NewPolyphaseFIR(44100, 48000)
	if err != nil {
		t.Fatal(err)
	}
	if fir.Q != 160 || fir.P != 147 {
		t.Fatalf("reduced ratio=%d/%d want147/160", fir.P, fir.Q)
	}
	center, phase := fir.Position(160)
	if center != 147 || phase != 0 {
		t.Fatalf("exact position(160)=%d,%d want147,0", center, phase)
	}

	in := sineAudio(44100, 44100, 2, 0.2)
	out, err := Resample(in, 48000)
	if err != nil {
		t.Fatal(err)
	}
	if out.Frames() != 48000 {
		t.Fatalf("resampled frames=%d want48000", out.Frames())
	}
	back, err := Resample(out, 44100)
	if err != nil {
		t.Fatal(err)
	}
	if back.Frames() != 44100 {
		t.Fatalf("roundtrip frames=%d want44100", back.Frames())
	}
}

func TestNormalizeLoudnessAndTruePeak(t *testing.T) {
	in := sineAudio(48000, 48000*3, 2, 0.5)
	out, rep, err := NormalizeToBroadcast(in, -23)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(rep.LUFSAfter+23) > 0.25 {
		t.Fatalf("LUFS after=%.3f want -23 ±0.25", rep.LUFSAfter)
	}
	if rep.TruePeakDBTPAfter > -0.95 {
		t.Fatalf("true peak=%.3f exceeds -1 dBTP", rep.TruePeakDBTPAfter)
	}
	if got := IntegratedLUFS(out); math.Abs(got+23) > 0.25 {
		t.Fatalf("remeasured LUFS=%.3f", got)
	}
}

func TestFindSilenceFindsConcealedPacketGap(t *testing.T) {
	a := sineAudio(48000, 48000, 2, 0.2)
	for c := 0; c < 2; c++ {
		for i := 1000; i < 1960; i++ {
			a.Channels[c][i] = 0
		}
	}
	regions := FindSilence(a)
	found := false
	for _, r := range regions {
		if r.StartFrame == 1000 && r.EndFrame == 1960 && r.EndFrame-r.StartFrame == 960 {
			found = true
		}
	}
	if !found {
		t.Fatalf("960-frame silence gap not found: %+v", regions)
	}
}

func TestL16RoundTrip(t *testing.T) {
	in := NewAudio(48000, 2, 1)
	in.Channels[0][0] = 0.5
	in.Channels[1][0] = -0.5
	b := EncodeS16LE(in)
	dec, err := DecodeL16BE(swapLEtoBE(b), 2)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(dec.Channels[0][0]-0.49997) > 0.001 || math.Abs(dec.Channels[1][0]+0.49997) > 0.001 {
		t.Fatalf("L16 roundtrip values=%v,%v", dec.Channels[0][0], dec.Channels[1][0])
	}
}

func swapLEtoBE(b []byte) []byte {
	out := append([]byte(nil), b...)
	for i := 0; i < len(out); i += 2 {
		out[i], out[i+1] = out[i+1], out[i]
	}
	return out
}
