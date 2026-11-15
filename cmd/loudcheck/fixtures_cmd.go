package main

import (
	"fmt"
	"os"
	"path/filepath"

	"loudcheck/internal/wavio"
)

func runFixtures(args []string) error {
	flags := parseFlags(args)
	out, ok := flags["out"]
	if !ok {
		return &exitError{exitUsage, "fixtures requires --out <dir>"}
	}
	seed := uint64(7)
	if value, exists := flags["seed"]; exists {
		var parsed uint64
		if _, err := fmt.Sscanf(value, "%d", &parsed); err != nil {
			return &exitError{exitUsage, "invalid --seed"}
		}
		seed = parsed
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	generator := fixtureGenerator{out: out, seed: seed}
	return generator.writeAll()
}

type fixtureGenerator struct {
	out  string
	seed uint64
}

func (g *fixtureGenerator) writeAll() error {
	targetLUFS := -23.0
	header48 := wavio.WriteHeader{SampleRate: 48000, Channels: 2, BitsPerSample: 24}
	tone := g.calibratedTone(48000, 20, 2, 1000, targetLUFS, header48)
	if err := wavio.WriteWAV(g.path("tone_1k_stereo_48k.wav"), header48, tone); err != nil {
		return err
	}
	if err := g.writeFormatFixtures(tone, header48); err != nil {
		return err
	}
	if err := g.writeOtherRates(targetLUFS); err != nil {
		return err
	}
	if err := g.writeSurround(targetLUFS); err != nil {
		return err
	}
	if err := g.writeFailures(targetLUFS); err != nil {
		return err
	}
	fmt.Println("generated 12 fixtures in", g.out)
	return nil
}

func (g *fixtureGenerator) path(name string) string {
	return filepath.Join(g.out, name)
}

func (g *fixtureGenerator) calibratedTone(sampleRate int, seconds, channels int, freq, targetLUFS float64,
	header wavio.WriteHeader) [][]float64 {
	frames := int(float64(sampleRate) * float64(seconds))
	burstLen := sampleRate / 4000
	burstStart := frames - sampleRate/2 - burstLen
	plateauStart := sampleRate * 3
	plateauEnd := sampleRate * (seconds - 3)
	build := func(amplitude, swellGain, burstAmp float64) [][]float64 {
		return g.compose(sampleRate, seconds, channels, freq, amplitude, swellGain, burstAmp,
			burstLen, burstStart, plateauStart, plateauEnd)
	}
	burstLo, burstHi := 0.7, 0.85
	var burstAmp float64
	for iteration := 0; iteration < 24; iteration++ {
		burstAmp = (burstLo + burstHi) / 2
		metric, err := measurePlanar(header, build(0.07, 0.12, burstAmp))
		if err != nil {
			burstHi = burstAmp
			continue
		}
		if metric.TruePeakDBTP < -1.8 {
			burstLo = burstAmp
		} else {
			burstHi = burstAmp
		}
	}
	burstAmp = (burstLo + burstHi) / 2
	swellLo, swellHi := 0.005, 0.06
	var swellGain float64
	for iteration := 0; iteration < 26; iteration++ {
		swellGain = (swellLo + swellHi) / 2
		probe := build(0.07, swellGain, burstAmp)
		metric, err := measurePlanar(header, probe)
		if err != nil {
			swellHi = swellGain
			continue
		}
		if metric.ShortTerm.MaxLUFS-metric.IntegratedLUFS < 0.10 {
			swellLo = swellGain
		} else {
			swellHi = swellGain
		}
	}
	swellGain = (swellLo + swellHi) / 2
	lo, hi := 0.001, 0.5
	var planar [][]float64
	for iteration := 0; iteration < 26; iteration++ {
		amplitude := (lo + hi) / 2
		planar = build(amplitude, swellGain, burstAmp)
		metric, err := measurePlanar(header, planar)
		if err != nil {
			hi = amplitude
			continue
		}
		if metric.IntegratedLUFS < targetLUFS {
			lo = amplitude
		} else {
			hi = amplitude
		}
	}
	_ = frames
	return planar
}

// compose layers the 1 kHz calibration tone, a continuous 6.2 kHz swell that
// only covers the central region (so a 3 s window sees more energy than the
// whole program), and a single short burst exercising the true-peak path.
func (g *fixtureGenerator) compose(sampleRate, seconds, channels int, freq, amplitude, swellGain,
	burstAmp float64, burstLen, burstStart, plateauStart, plateauEnd int) [][]float64 {
	planar := wavio.TonePlanar(sampleRate, float64(seconds), channels, freq, amplitude, g.seedPhase(freq))
	swell := wavio.TonePlanar(sampleRate, float64(seconds), channels, 6200,
		amplitude*swellGain, g.seedPhase(6200))
	for ch := range planar {
		for n := range planar[ch] {
			if n >= plateauStart && n < plateauEnd {
				planar[ch][n] += swell[ch][n]
			}
		}
	}
	burst := wavio.TonePlanar(sampleRate, float64(burstLen)/float64(sampleRate), channels,
		8000, burstAmp, g.seedPhase(6200))
	wavio.MixInto(planar, burstStart, burst)
	return planar
}

func (g *fixtureGenerator) seedPhase(freq float64) float64 {
	return float64(g.seed%97) * 0.013 + freq*0.0001
}

func clonePlanar(planar [][]float64) [][]float64 {
	out := make([][]float64, len(planar))
	for ch := range planar {
		out[ch] = append([]float64(nil), planar[ch]...)
	}
	return out
}

func replacePlanar(dst, src [][]float64) {
	for ch := range dst {
		copy(dst[ch], src[ch])
	}
}
