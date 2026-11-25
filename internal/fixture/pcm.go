package fixture

import (
	"math"

	"github.com/fieldcut/fieldcut/internal/audio"
)

// PCMParams describes a generated source PCM block.
type PCMParams struct {
	SampleRate uint32
	Layout     audio.Layout
	Seconds    float64
	FreqHz     float64 // tone frequency for deterministic, non-silent content
	Seed       uint64
}

// GeneratePCM synthesizes a loudness-stable tone plus a deterministic
// amplitude envelope. 5.1 sources place content on discrete channels so the
// downmix mapping is exercised; a quiet passage covers the gating path.
func GeneratePCM(p PCMParams) *audio.Frame {
	ch := p.Layout.Channels()
	n := int(math.Round(float64(p.SampleRate) * p.Seconds))
	samples := make([]float32, n*ch)
	rng := newRNG(p.Seed)
	for i := 0; i < n; i++ {
		t := float64(i) / float64(p.SampleRate)
		// Two-tone mix avoids a single-sine crest factor and stabilizes the
		// BS.1770 measurement across resampling ratios.
		v := 0.6*math.Sin(2*math.Pi*p.FreqHz*t) +
			0.3*math.Sin(2*math.Pi*p.FreqHz*1.5*t)
		// One quiet 300 ms passage every 4 s exercises silence gating.
		phase := math.Mod(t, 4.0)
		if phase < 0.3 {
			v *= 0.02
		}
		jitter := 1.0 - 0.01*float64(rng.intn(2))
		v *= jitter
		for c := 0; c < ch; c++ {
			samples[i*ch+c] = channelGain(p.Layout, c, v)
		}
	}
	return &audio.Frame{SampleRate: p.SampleRate, Layout: p.Layout, Samples: samples}
}

// channelGain distributes the tone across a layout with an explicit,
// documented per-channel level so no channel is silently dropped.
func channelGain(layout audio.Layout, ch int, v float64) float32 {
	switch layout {
	case audio.LayoutMono:
		return float32(clamp(v * 0.9))
	case audio.LayoutStereo:
		if ch == 0 {
			return float32(clamp(v * 0.85))
		}
		return float32(clamp(v * 0.75))
	case audio.Layout51:
		switch ch {
		case 0: // L
			return float32(clamp(v * 0.8))
		case 1: // R
			return float32(clamp(v * 0.7))
		case 2: // C
			return float32(clamp(v * 0.6))
		case 3: // LFE, intentionally separate, tracked via LFEPeak
			return float32(clamp(v * 0.5))
		case 4: // Ls
			return float32(clamp(v * 0.45))
		case 5: // Rs
			return float32(clamp(v * 0.4))
		}
	}
	return 0
}

func clamp(v float64) float64 {
	if v > 1 {
		return 1
	}
	if v < -1 {
		return -1
	}
	return v
}

// GeneratePCMSuite returns one source per supported rate/layout combination
// so the audio tests cover every normalization branch.
func GeneratePCMSuite(seed uint64, seconds float64) []*audio.Frame {
	return []*audio.Frame{
		GeneratePCM(PCMParams{SampleRate: 48000, Layout: audio.Layout51, Seconds: seconds, FreqHz: 440, Seed: seed}),
		GeneratePCM(PCMParams{SampleRate: 44100, Layout: audio.LayoutMono, Seconds: seconds, FreqHz: 330, Seed: seed + 1}),
		GeneratePCM(PCMParams{SampleRate: 48000, Layout: audio.LayoutStereo, Seconds: seconds, FreqHz: 550, Seed: seed + 2}),
	}
}
