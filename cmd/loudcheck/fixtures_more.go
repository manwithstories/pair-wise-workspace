package main

import "loudcheck/internal/wavio"

func (g *fixtureGenerator) writeFormatFixtures(tone [][]float64, base wavio.WriteHeader) error {
	formats := []struct {
		name string
		hdr  wavio.WriteHeader
	}{
		{"tone_1k_stereo_48k_pcm16.wav", wavio.WriteHeader{SampleRate: 48000, Channels: 2, BitsPerSample: 16}},
		{"tone_1k_stereo_48k_float32.wav", wavio.WriteHeader{SampleRate: 48000, Channels: 2, BitsPerSample: 32, IsFloat: true}},
		{"tone_1k_stereo_48k_float64.wav", wavio.WriteHeader{SampleRate: 48000, Channels: 2, BitsPerSample: 64, IsFloat: true}},
	}
	for _, item := range formats {
		if err := wavio.WriteWAV(g.path(item.name), item.hdr, tone); err != nil {
			return err
		}
	}
	return nil
}

func (g *fixtureGenerator) writeOtherRates(targetLUFS float64) error {
	monoHeader := wavio.WriteHeader{SampleRate: 48000, Channels: 1, BitsPerSample: 16}
	mono := g.calibratedTone(48000, 12, 1, 1000, targetLUFS, monoHeader)
	if err := wavio.WriteWAV(g.path("tone_1k_mono_48k.wav"), monoHeader, mono); err != nil {
		return err
	}
	header44 := wavio.WriteHeader{SampleRate: 44100, Channels: 2, BitsPerSample: 24}
	tone44 := g.calibratedTone(44100, 15, 2, 1000, targetLUFS, header44)
	if err := wavio.WriteWAV(g.path("tone_1k_stereo_44k1.wav"), header44, tone44); err != nil {
		return err
	}
	header96 := wavio.WriteHeader{SampleRate: 96000, Channels: 2, BitsPerSample: 32}
	tone96 := g.calibratedTone(96000, 10, 2, 1000, targetLUFS, header96)
	if err := wavio.WriteWAV(g.path("tone_1k_stereo_96k.wav"), header96, tone96); err != nil {
		return err
	}
	pinkHeader := wavio.WriteHeader{SampleRate: 48000, Channels: 2, BitsPerSample: 24}
	pink := g.calibratedPink(48000, 20, 2, targetLUFS, pinkHeader)
	return wavio.WriteWAV(g.path("pinknoise_stereo_48k.wav"), pinkHeader, pink)
}

func (g *fixtureGenerator) calibratedPink(sampleRate, seconds, channels int, targetLUFS float64,
	header wavio.WriteHeader) [][]float64 {
	lo, hi := 0.001, 0.8
	var planar [][]float64
	for iteration := 0; iteration < 24; iteration++ {
		amplitude := (lo + hi) / 2
		planar = wavio.PinkPlanar(sampleRate, float64(seconds), channels, amplitude, g.seed+uint64(iteration))
		metric, err := measurePlanar(header, planar)
		if err != nil || metric.IntegratedLUFS < targetLUFS {
			lo = amplitude
		} else {
			hi = amplitude
		}
	}
	return planar
}

func (g *fixtureGenerator) writeSurround(targetLUFS float64) error {
	const mask = 0x1 | 0x2 | 0x4 | 0x8 | 0x20 | 0x40
	header := wavio.WriteHeader{
		SampleRate: 48000, Channels: 6, BitsPerSample: 24,
		UseExtensible: true, ChannelMask: mask,
	}
	planar := g.calibratedPink(48000, 16, 6, targetLUFS, header)
	for n := range planar[3] {
		planar[3][n] *= 0.25
	}
	return wavio.WriteWAV(g.path("pinknoise_5dot1_48k.wav"), header, planar)
}

func (g *fixtureGenerator) writeFailures(targetLUFS float64) error {
	quietHeader := wavio.WriteHeader{SampleRate: 48000, Channels: 2, BitsPerSample: 24}
	quiet := g.calibratedTone(48000, 10, 2, 1000, targetLUFS-4, quietHeader)
	if err := wavio.WriteWAV(g.path("quiet_stereo_48k.wav"), quietHeader, quiet); err != nil {
		return err
	}
	hotHeader := wavio.WriteHeader{SampleRate: 48000, Channels: 2, BitsPerSample: 24}
	hot := g.calibratedTone(48000, 8, 2, 1000, -18, hotHeader)
	if err := wavio.WriteWAV(g.path("hot_stereo_48k.wav"), hotHeader, hot); err != nil {
		return err
	}
	truncHeader := wavio.WriteHeader{SampleRate: 48000, Channels: 2, BitsPerSample: 24}
	trunc := wavio.TonePlanar(48000, 6, 2, 1000, 0.2, 0.4)
	return wavio.WriteTruncated(g.path("truncated_24bit.wav"), truncHeader, trunc, 4096)
}
