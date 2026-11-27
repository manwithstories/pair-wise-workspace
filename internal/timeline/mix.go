// Package timeline resamples tracks onto the unified master rate, applies
// channel matrices and mixes them, and resolves conflicting color metadata
// deterministically.
package timeline

import (
	"fmt"
	"sort"

	"avcheck/internal/clock"
	"avcheck/internal/fault"
	"avcheck/internal/wav"
)

// Rate is the unified timeline sample rate.
const Rate = 48000

// ColorMeta is a track's declared color metadata.
type ColorMeta struct{ Primaries, Transfer string }

var knownPrimaries = map[string]bool{"bt709": true, "bt2020": true, "srgb": true}
var knownTransfer = map[string]bool{"gamma22": true, "pq": true, "hlg": true, "linear": true}

// Segment is a run of samples placed at a master-clock instant. Recovered
// gaps are simply absent segments, so nothing is silently shifted.
type Segment struct {
	Start clock.Tick
	PCM   *wav.PCM
}

// Track is one timeline input.
type Track struct {
	Name     string
	Segments []Segment
	Color    ColorMeta
}

// Config is the resolved timeline configuration.
type Config struct {
	Primaries string
	Transfer  string
	Conflicts []string // human-readable record of resolved conflicts
}

// ResolveConfig validates every track's color metadata and picks the
// timeline config deterministically: the most frequent (primaries,transfer)
// pair wins, ties broken by lexicographic order. Every divergence from the
// winner is recorded. Unknown values take the explicit error path.
func ResolveConfig(tracks []Track) (Config, error) {
	const op = "timeline.config"
	counts := map[[2]string]int{}
	for i, tr := range tracks {
		if !knownPrimaries[tr.Color.Primaries] {
			return Config{}, fault.New(fault.Unsupported, op, int64(i),
				"unknown primaries "+tr.Color.Primaries)
		}
		if !knownTransfer[tr.Color.Transfer] {
			return Config{}, fault.New(fault.Unsupported, op, int64(i),
				"unknown transfer "+tr.Color.Transfer)
		}
		counts[[2]string{tr.Color.Primaries, tr.Color.Transfer}]++
	}
	type pair struct {
		k [2]string
		n int
	}
	var ps []pair
	for k, n := range counts {
		ps = append(ps, pair{k, n})
	}
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].n != ps[j].n {
			return ps[i].n > ps[j].n
		}
		a, b := ps[i].k, ps[j].k
		return a[0] < b[0] || (a[0] == b[0] && a[1] < b[1])
	})
	if len(ps) == 0 {
		return Config{}, fault.New(fault.Unsupported, op, -1, "no tracks")
	}
	win := ps[0].k
	cfg := Config{Primaries: win[0], Transfer: win[1]}
	for _, p := range ps[1:] {
		cfg.Conflicts = append(cfg.Conflicts, fmt.Sprintf(
			"color conflict: %s/%s (%d track(s)) dropped in favour of %s/%s",
			p.k[0], p.k[1], p.n, win[0], win[1]))
	}
	return cfg, nil
}

// Mix renders all tracks to interleaved stereo at the unified rate, spanning
// [0, end). Gains come from each segment's layout matrix; the sum is
// normalized by 1/sqrt(track count) and hard-clamped to int16.
func Mix(tracks []Track, end clock.Tick) []int16 {
	n := int(clock.ToSamples(end, Rate))
	out := make([]float64, n*2)
	norm := 1.0
	if len(tracks) > 0 {
		norm = 1.0 / sqrt(float64(len(tracks)))
	}
	for _, tr := range tracks {
		for _, seg := range tr.Segments {
			mixSegment(out, seg, norm)
		}
	}
	res := make([]int16, n*2)
	for i, v := range out {
		if v > 32767 {
			v = 32767
		} else if v < -32768 {
			v = -32768
		}
		res[i] = int16(v)
	}
	return res
}

// mixSegment resamples one segment linearly onto the master rate and adds it
// into out at the segment's tick position. For each output frame it samples
// the source at the exact fractional position, so energy is preserved when
// both upsampling and downsampling.
func mixSegment(out []float64, seg Segment, norm float64) {
	p := seg.PCM
	ch := p.Layout.Channels()
	mtx := p.Layout.Matrix()
	if ch == 0 || len(mtx) != ch || p.Frames() < 2 {
		return
	}
	frames := p.Frames()
	start := clock.ToSamples(seg.Start, Rate)
	outFrames := int(clock.ToSamples(clock.FromSamples(int64(frames), p.Rate), Rate))
	limit := int64(len(out) / 2)
	for j := 0; j < outFrames; j++ {
		dst := start + int64(j)
		if dst < 0 || dst >= limit {
			continue
		}
		pos := float64(j) * float64(p.Rate) / float64(Rate)
		i0 := int(pos)
		if i0 > frames-2 {
			i0 = frames - 2
		}
		f := pos - float64(i0)
		var l, r float64
		for c := 0; c < ch; c++ {
			s := float64(p.Samples[i0*ch+c])*(1-f) + float64(p.Samples[(i0+1)*ch+c])*f
			l += s * mtx[c][0]
			r += s * mtx[c][1]
		}
		out[dst*2] += l * norm
		out[dst*2+1] += r * norm
	}
}

func sqrt(x float64) float64 {
	// Newton's method, avoids importing math for one call in hot path docs.
	if x <= 0 {
		return 0
	}
	z := x
	for i := 0; i < 32; i++ {
		z -= (z*z - x) / (2 * z)
	}
	return z
}
