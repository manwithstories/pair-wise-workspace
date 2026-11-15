package timeline

import (
	"math/big"
	"sort"

	"subtool/internal/model"
)

// OverlapStats records what overlap resolution did to a cue set.
type OverlapStats struct {
	Splits    int      // cues split into segments
	Trims     int      // cues whose end was trimmed
	TrimmedMs *big.Rat // total trimmed duration
	Relayered int      // cues moved to a higher layer
}

// ResolveMode selects the overlap strategy.
type ResolveMode string

const (
	// ModeLayer keeps dual-speaker overlaps by stacking layers.
	ModeLayer ResolveMode = "layer"
	// ModeTrim cuts the earlier cue's end at the next cue's start.
	ModeTrim ResolveMode = "trim"
	// ModeSplit splits cues at every overlap boundary.
	ModeSplit ResolveMode = "split"
)

// Resolve applies the overlap strategy to cues (sorted copy returned).
// Cues merely touching at one frame boundary (end == start) are not
// overlaps and are never merged.
func Resolve(cues []model.Cue, mode ResolveMode) ([]model.Cue, OverlapStats) {
	stats := OverlapStats{TrimmedMs: new(big.Rat)}
	out := make([]model.Cue, len(cues))
	for i, c := range cues {
		out[i] = c.Clone()
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Layer != out[j].Layer {
			return out[i].Layer < out[j].Layer
		}
		if cmp := out[i].Start.Cmp(out[j].Start); cmp != 0 {
			return cmp < 0
		}
		return out[i].ID < out[j].ID
	})
	switch mode {
	case ModeTrim:
		out = resolveTrim(out, &stats)
	case ModeSplit:
		out = resolveSplit(out, &stats)
	default:
		out = resolveLayer(out, &stats)
	}
	model.SortByStart(out)
	return out, stats
}

// resolveLayer keeps every cue but bumps overlapping cues onto free
// layers so dual-speaker passages coexist on parallel tracks.
func resolveLayer(cues []model.Cue, stats *OverlapStats) []model.Cue {
	// layerEnds[l] = latest end time currently occupying layer l.
	layerEnds := map[int]*big.Rat{}
	for i := range cues {
		l := cues[i].Layer
		for {
			end, busy := layerEnds[l]
			if !busy || end.Cmp(cues[i].Start) <= 0 {
				break
			}
			l++
		}
		if l != cues[i].Layer {
			stats.Relayered++
			cues[i].Layer = l
		}
		if cur := layerEnds[l]; cur == nil || cues[i].End.Cmp(cur) > 0 {
			layerEnds[l] = cues[i].End
		}
	}
	return cues
}

// resolveTrim shortens the earlier of two overlapping cues on the same
// layer; a cue trimmed to zero length is dropped and counted.
func resolveTrim(cues []model.Cue, stats *OverlapStats) []model.Cue {
	byLayer := map[int][]int{}
	var layers []int
	for i, c := range cues {
		if _, ok := byLayer[c.Layer]; !ok {
			layers = append(layers, c.Layer)
		}
		byLayer[c.Layer] = append(byLayer[c.Layer], i)
	}
	dropped := map[int]bool{}
	for _, idxs := range byLayer {
		sort.Slice(idxs, func(a, b int) bool {
			return cues[idxs[a]].Start.Cmp(cues[idxs[b]].Start) < 0
		})
		for k := 0; k+1 < len(idxs); k++ {
			a, b := cues[idxs[k]], cues[idxs[k+1]]
			if a.End.Cmp(b.Start) > 0 {
				trim := new(big.Rat).Sub(a.End, b.Start)
				stats.TrimmedMs.Add(stats.TrimmedMs, trim)
				stats.Trims++
				cues[idxs[k]].End = b.Start
				if cues[idxs[k]].End.Cmp(cues[idxs[k]].Start) <= 0 {
					dropped[idxs[k]] = true
				}
			}
		}
	}
	out := cues[:0]
	for i, c := range cues {
		if !dropped[i] {
			out = append(out, c)
		}
	}
	return out
}

// resolveSplit cuts cues at every distinct overlap boundary so no two
// emitted segments on one layer intersect; touching segments stay
// separate (never merged).
func resolveSplit(cues []model.Cue, stats *OverlapStats) []model.Cue {
	var out []model.Cue
	for _, c := range cues {
		points := map[string]*big.Rat{
			c.Start.String(): c.Start,
			c.End.String():   c.End,
		}
		for _, o := range cues {
			if o.Layer != c.Layer || o.ID == c.ID {
				continue
			}
			if o.Start.Cmp(c.Start) > 0 && o.Start.Cmp(c.End) < 0 {
				points[o.Start.String()] = o.Start
			}
			if o.End.Cmp(c.Start) > 0 && o.End.Cmp(c.End) < 0 {
				points[o.End.String()] = o.End
			}
		}
		pts := make([]*big.Rat, 0, len(points))
		for _, p := range points {
			pts = append(pts, p)
		}
		sort.Slice(pts, func(a, b int) bool { return pts[a].Cmp(pts[b]) < 0 })
		if len(pts) > 2 {
			stats.Splits += len(pts) - 2
		}
		for k := 0; k+1 < len(pts); k++ {
			seg := c.Clone()
			seg.Start, seg.End = pts[k], pts[k+1]
			out = append(out, seg)
		}
	}
	return out
}
