package audio

import (
	"math"
	"testing"
)

func TestResolveLayoutDeclared(t *testing.T) {
	cases := []struct {
		cfg      Config
		resolved string
		fallback bool
	}{
		{Config{SampleRate: 48000, Channels: 2, Layout: "stereo"}, LayoutStereo, false},
		{Config{SampleRate: 44100, Channels: 1, Layout: "mono"}, LayoutMono, false},
		{Config{SampleRate: 48000, Channels: 6, Layout: "5.1"}, Layout51, false},
	}
	for _, c := range cases {
		d, err := ResolveLayout(c.cfg)
		if err != nil {
			t.Fatalf("%+v: %v", c.cfg, err)
		}
		if d.Resolved != c.resolved || d.Fallback != c.fallback {
			t.Fatalf("%+v: want %s/%v got %+v", c.cfg, c.resolved, c.fallback, d)
		}
	}
}

func TestResolveLayoutFallbackIsExplicit(t *testing.T) {
	cases := []struct {
		cfg      Config
		resolved string
	}{
		{Config{SampleRate: 48000, Channels: 2, Layout: ""}, LayoutStereo},
		{Config{SampleRate: 48000, Channels: 2, Layout: "unknown"}, LayoutStereo},
		{Config{SampleRate: 48000, Channels: 1, Layout: "missing"}, LayoutMono},
		{Config{SampleRate: 48000, Channels: 4, Layout: "quad"}, LayoutUnknown},
	}
	for _, c := range cases {
		d, err := ResolveLayout(c.cfg)
		if err != nil {
			t.Fatalf("%+v: %v", c.cfg, err)
		}
		if d.Resolved != c.resolved {
			t.Fatalf("%+v: want %s got %s", c.cfg, c.resolved, d.Resolved)
		}
		if !d.Fallback {
			t.Fatalf("%+v: fallback must be flagged", c.cfg)
		}
		if d.Reason == "" {
			t.Fatalf("%+v: fallback reason must be recorded", c.cfg)
		}
	}
}

func TestResolveLayoutContradiction(t *testing.T) {
	_, err := ResolveLayout(Config{SampleRate: 48000, Channels: 3, Layout: "stereo"})
	if err == nil {
		t.Fatal("want contradiction error")
	}
}

func TestMonoUpmixToStereo(t *testing.T) {
	src := make([]float32, 4800) // 100 ms mono
	for i := range src {
		src[i] = float32(math.Sin(2 * math.Pi * 440 * float64(i) / 48000))
	}
	out, decision, err := Convert(Config{SampleRate: 48000, Channels: 1, Layout: "mono"}, src)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Fallback {
		t.Fatal("declared mono is not a fallback")
	}
	if len(out) != len(src)*2 {
		t.Fatalf("want %d stereo samples got %d", len(src)*2, len(out))
	}
	for i := 0; i < len(src); i++ {
		if out[i*2] != out[i*2+1] {
			t.Fatalf("mono channel not duplicated at frame %d", i)
		}
	}
}

func TestResampleFrom44k1(t *testing.T) {
	const rate = 44100
	n := rate / 10 // 100 ms
	src := make([]float32, n*2)
	for i := 0; i < n; i++ {
		v := float32(0.5 * math.Sin(2*math.Pi*1000*float64(i)/rate))
		src[i*2], src[i*2+1] = v, v
	}
	out, _, err := Convert(Config{SampleRate: rate, Channels: 2, Layout: "stereo"}, src)
	if err != nil {
		t.Fatal(err)
	}
	wantFrames := 4800
	if len(out) != wantFrames*2 {
		t.Fatalf("want %d frames got %d", wantFrames, len(out)/2)
	}
	// Energy should be preserved within 10%.
	before := energy(src)
	after := energy(out)
	if math.Abs(before-after)/before > 0.10 {
		t.Fatalf("energy drift too high: before=%f after=%f", before, after)
	}
	// No clipping after resampling.
	for _, v := range out {
		if v > 1.0 || v < -1.0 {
			t.Fatalf("resample produced out-of-range sample %f", v)
		}
	}
}

func energy(x []float32) float64 {
	var s float64
	for _, v := range x {
		s += float64(v) * float64(v)
	}
	return s
}

func TestNormalizeHitsLoudnessAndPeakTargets(t *testing.T) {
	// 2 s of quiet stereo noise.
	n := TargetRate * 2
	src := make([]float32, n*2)
	seed := uint32(12345)
	for i := range src {
		seed = seed*1664525 + 1013904223
		v := (float32(seed>>8)/float32(1<<24) - 0.5) * 0.1
		src[i] = v
	}
	res := Normalize(src, 400)
	if !res.AppliedLoudness {
		t.Fatal("loudness was not applied")
	}
	if math.Abs(res.OutputLUFS-TargetLUFS) > 1.0 {
		t.Fatalf("output loudness %f too far from target %f", res.OutputLUFS, TargetLUFS)
	}
	if res.TruePeakDBTP > MaxTruePeakDBTP+0.01 {
		t.Fatalf("true peak %f dBTP exceeds ceiling %f", res.TruePeakDBTP, MaxTruePeakDBTP)
	}
	if res.GainDB <= 0 {
		t.Fatalf("quiet material should be boosted, gain=%f", res.GainDB)
	}
}

func TestNormalizeLimitsLoudMaterial(t *testing.T) {
	// Low RMS (quiet tone) with isolated full-scale impulses: normalising
	// loudness to -16 LUFS would push the peaks far above the ceiling.
	n := TargetRate * 2
	src := make([]float32, n*2)
	for i := 0; i < n; i++ {
		v := float32(0.01 * math.Sin(2*math.Pi*1000*float64(i)/TargetRate))
		if i%TargetRate == 0 {
			v = 0.98
		}
		src[i*2], src[i*2+1] = v, v
	}
	res := Normalize(src, 400)
	if res.TruePeakDBTP > MaxTruePeakDBTP+0.01 {
		t.Fatalf("peak %f exceeds ceiling", res.TruePeakDBTP)
	}
	if !res.Limited {
		t.Fatal("loud material must trigger the limiter")
	}
}

func TestTruePeakDetectsIntersample(t *testing.T) {
	// A quarter-rate sine sampled at 45-degree phase has a true peak of 1.0
	// while every sample sits at 0.707.
	frames := 256
	src := make([]float32, frames*2)
	for i := 0; i < frames; i++ {
		v := float32(math.Sin(math.Pi*float64(i)/2 + math.Pi/4))
		src[i*2], src[i*2+1] = v, v
	}
	p := TruePeakDBTP(src)
	if p < -0.5 {
		t.Fatalf("intersample peak not detected: %f dBTP", p)
	}
}
