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
					// Cosine burst: full amplitude at the onset sample so
					// threshold detection localizes the onset to <=1 sample.
					v += 12000 * math.Cos(2*math.Pi*880*d)
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

// FlushBlocks is the number of redundancy-only blocks appended at the end of
// each track so the tail audio blocks have forward carriers too.
const FlushBlocks = 4

// cutBlocks slices PCM into BlockMS blocks, each carrying redundancy copies
// of the four preceding blocks, then appends FlushBlocks empty tail blocks
// that exist purely to carry redundancy for the tail of the stream.
func cutBlocks(p *wav.PCM) []*wav.Block {
	ch := p.Layout.Channels()
	per := p.Rate * BlockMS / 1000 * ch
	var blocks []*wav.Block
	for off := 0; off < len(p.Samples); off += per {
		end := off + per
		if end > len(p.Samples) {
			end = len(p.Samples)
		}
		blocks = append(blocks, &wav.Block{Seq: uint64(len(blocks)), Rate: p.Rate,
			Layout: p.Layout, Samples: p.Samples[off:end]})
	}
	for i := 0; i < FlushBlocks; i++ {
		blocks = append(blocks, &wav.Block{Seq: uint64(len(blocks)), Rate: p.Rate,
			Layout: p.Layout, Samples: []int16{}})
	}
	for i, b := range blocks {
		for back := 1; back <= 4 && i-back >= 0; back++ {
			b.Red[back-1] = wav.Redundancy{Seq: b.Seq - uint64(back),
				Samples: blocks[i-back].Samples}
		}
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
