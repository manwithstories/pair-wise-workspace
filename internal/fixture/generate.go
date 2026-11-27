// Package fixture generates the demo's source material deterministically:
// four 10-second tracks at different rates and layouts, shared event beeps,
// conflicting color metadata, and a simulated lossy network transport.
package fixture

import (
	"math"
	"math/rand"

	"avcheck/internal/clock"
	"avcheck/internal/render"
	"avcheck/internal/timeline"
	"avcheck/internal/wav"
)

// DurationS is the material length in seconds.
const DurationS = 10

// BlockMS is the network block size in milliseconds.
const BlockMS = 20

// EventTimesS are the beep onsets shared by every track (the A/V sync
// reference). Video markers and audio beeps are generated from the same
// instants, so any measured offset is pipeline error.
var EventTimesS = []float64{1.0, 4.5, 7.0}

// TrackData is one generated track with its blocks.
type TrackData struct {
	Name   string
	Rate   int
	Layout wav.Layout
	Color  timeline.ColorMeta
	PCM    *wav.PCM
	Blocks []*wav.Block
}

// Source is the full generated input.
type Source struct {
	Tracks   []TrackData
	Events   []render.Event
	Duration clock.Tick
}

// Generate builds the deterministic source material.
func Generate() *Source {
	specs := []struct {
		name   string
		rate   int
		layout wav.Layout
		freq   float64
		color  timeline.ColorMeta
	}{
		{"alpha", 44100, wav.Stereo, 220, timeline.ColorMeta{Primaries: "bt709", Transfer: "gamma22"}},
		{"bravo", 48000, wav.Stereo, 330, timeline.ColorMeta{Primaries: "bt709", Transfer: "gamma22"}},
		{"carlo", 96000, wav.Mono, 440, timeline.ColorMeta{Primaries: "bt2020", Transfer: "pq"}},
		{"delta", 48000, wav.Quad, 550, timeline.ColorMeta{Primaries: "bt709", Transfer: "gamma22"}},
	}
	src := &Source{Duration: clock.FromSamples(int64(DurationS*48000), 48000)}
	for _, et := range EventTimesS {
		src.Events = append(src.Events, render.Event{Tick: clock.FromMillis(et * 1000), Label: "beep"})
	}
	for _, sp := range specs {
		frames := sp.rate * DurationS
		ch := sp.layout.Channels()
		samples := make([]int16, frames*ch)
		for i := 0; i < frames; i++ {
			t := float64(i) / float64(sp.rate)
			v := 3000 * math.Sin(2*math.Pi*sp.freq*t)
			for _, et := range EventTimesS {
				if d := t - et; d >= 0 && d < 0.05 {
					v += 12000 * math.Sin(2*math.Pi*880*t)
				}
			}
			s := int16(v)
			for c := 0; c < ch; c++ {
				samples[i*ch+c] = s
			}
		}
		pcm := &wav.PCM{Rate: sp.rate, Layout: sp.layout, Samples: samples}
		src.Tracks = append(src.Tracks, TrackData{
			Name: sp.name, Rate: sp.rate, Layout: sp.layout,
			Color: sp.color, PCM: pcm, Blocks: cutBlocks(pcm),
		})
	}
	return src
}

// cutBlocks slices PCM into BlockMS blocks, each carrying redundancy copies
// of the two preceding blocks.
func cutBlocks(p *wav.PCM) []*wav.Block {
	ch := p.Layout.Channels()
	per := p.Rate * BlockMS / 1000 * ch
	var blocks []*wav.Block
	for off, seq := 0, uint64(0); off < len(p.Samples); off, seq = off+per, seq+1 {
		end := off + per
		if end > len(p.Samples) {
			end = len(p.Samples)
		}
		b := &wav.Block{Seq: seq, Rate: p.Rate, Layout: p.Layout,
			Samples: p.Samples[off:end]}
		if len(blocks) > 0 {
			b.Red[0] = wav.Redundancy{Seq: seq - 1, Samples: blocks[len(blocks)-1].Samples}
		}
		if len(blocks) > 1 {
			b.Red[1] = wav.Redundancy{Seq: seq - 2, Samples: blocks[len(blocks)-2].Samples}
		}
		blocks = append(blocks, b)
	}
	return blocks
}

// Transmission is the simulated arrival of one track's blocks.
type Transmission struct {
	Arrived   []*wav.Block // out of order, with duplicates, minus losses
	Latencies []float64    // simulated end-to-end ms per arrived block
	Dropped   int
}

// Transmit simulates a mobile link: independent loss, reordering,
// duplication and jittered latency. Deterministic for a given seed.
func Transmit(blocks []*wav.Block, seed int64, loss, jitterMs float64) *Transmission {
	rng := rand.New(rand.NewSource(seed))
	tr := &Transmission{}
	for _, b := range blocks {
		if rng.Float64() < loss {
			tr.Dropped++
			continue
		}
		tr.Arrived = append(tr.Arrived, b)
		tr.Latencies = append(tr.Latencies, 40+rng.Float64()*2*jitterMs)
		if rng.Float64() < 0.02 { // occasional duplicate delivery
			tr.Arrived = append(tr.Arrived, b)
			tr.Latencies = append(tr.Latencies, 40+rng.Float64()*2*jitterMs)
		}
	}
	rng.Shuffle(len(tr.Arrived), func(i, j int) {
		tr.Arrived[i], tr.Arrived[j] = tr.Arrived[j], tr.Arrived[i]
	})
	return tr
}
