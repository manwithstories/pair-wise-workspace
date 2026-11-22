// Package ladder aligns representation boundaries on a shared time grid.
// Every representation is cut at the same grid instants so a player can switch
// bitrate at any boundary. When a representation's packets do not divide the
// grid exactly, the deviation is recorded on the segment instead of being
// silently rounded away.
package ladder

import (
	"fmt"
	"sort"
	"strings"

	"example.com/medialab/archive/internal/model"
)

// ParseBitrates parses a comma separated list such as "64k,128k" into a sorted
// ladder of at least two representations.
func ParseBitrates(s string) ([]model.Representation, error) {
	var reps []model.Representation
	seen := map[int]bool{}
	for _, raw := range strings.Split(s, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		var kbps int
		if _, err := fmt.Sscanf(strings.ToLower(raw), "%dk", &kbps); err != nil || kbps <= 0 {
			return nil, fmt.Errorf("ladder: invalid bitrate %q (want e.g. 64k)", raw)
		}
		if seen[kbps] {
			return nil, fmt.Errorf("ladder: duplicate bitrate %dk", kbps)
		}
		seen[kbps] = true
		reps = append(reps, model.Representation{
			ID:      model.RepresentationID(fmt.Sprintf("%dk", kbps)),
			Bitrate: kbps * 1000,
		})
	}
	if len(reps) < 2 {
		return nil, fmt.Errorf("ladder: at least two representations are required, got %d", len(reps))
	}
	sort.Slice(reps, func(i, j int) bool {
		if reps[i].Bitrate != reps[j].Bitrate {
			return reps[i].Bitrate < reps[j].Bitrate
		}
		return reps[i].ID < reps[j].ID
	})
	return reps, nil
}

// FrameDurationFor converts a frame size in samples to microseconds, truncating
// consistently with the packet duration derivation.
func FrameDurationFor(sampleRate, samplesPerFrame int) (int64, error) {
	if sampleRate <= 0 || samplesPerFrame <= 0 {
		return 0, fmt.Errorf("ladder: invalid frame geometry %d/%d", sampleRate, samplesPerFrame)
	}
	us := int64(samplesPerFrame) * 1_000_000 / int64(sampleRate)
	if us <= 0 {
		return 0, fmt.Errorf("ladder: frame duration underflows")
	}
	return us, nil
}

// Grid returns the shared cut instants. The last instants is durationUS itself,
// never a padded grid multiple.
func Grid(durationUS, segmentUS int64) []int64 {
	if durationUS <= 0 || segmentUS <= 0 {
		return nil
	}
	var out []int64
	for t := segmentUS; t < durationUS; t += segmentUS {
		out = append(out, t)
	}
	return append(out, durationUS)
}

// SegmentWindow is one aligned slice of one representation.
type SegmentWindow struct {
	Rep        model.RepresentationID
	Index      int
	StartUS    int64
	EndUS      int64
	IsFinal    bool
	FrameCount int
	// GridSkewUS is the distance from the grid boundary to the nearest packet
	// edge of this representation (0 when packets divide the grid).
	GridSkewUS int64
	// EdgeTrimUS is data trimmed at the common duration for this rep; positive
	// means the representation carried audio past the shared end.
	EdgeTrimUS int64
}

// Align cuts every representation at the shared boundaries. edges maps each
// representation to the observed packet start/end instants. commonUS is the
// duration shared by all representations; anything past it is trimmed and
// recorded. It returns the windows and the worst packet-to-grid skew.
func Align(reps []model.Representation, edges map[model.RepresentationID][]int64, commonUS, segmentUS int64) ([]SegmentWindow, int64, error) {
	if commonUS <= 0 {
		return nil, 0, fmt.Errorf("ladder: non-positive duration %d", commonUS)
	}
	if segmentUS <= 0 {
		return nil, 0, fmt.Errorf("ladder: non-positive segment %d", segmentUS)
	}
	if len(reps) < 2 {
		return nil, 0, fmt.Errorf("ladder: need at least two representations")
	}
	boundaries := Grid(commonUS, segmentUS)
	var windows []SegmentWindow
	maxSkew := int64(0)
	for _, rep := range reps {
		repEdges := append([]int64(nil), edges[rep.ID]...)
		sort.Slice(repEdges, func(i, j int) bool { return repEdges[i] < repEdges[j] })
		lastEdge := int64(0)
		if len(repEdges) > 0 {
			lastEdge = repEdges[len(repEdges)-1]
		}
		trim := lastEdge - commonUS
		if trim < 0 {
			trim = 0
		}
		prev := int64(0)
		for i, b := range boundaries {
			if b <= prev {
				return nil, 0, fmt.Errorf("ladder: empty window %s/%d", rep.ID, i)
			}
			skew := nearestSkew(repEdges, b)
			if skew > maxSkew {
				maxSkew = skew
			}
			windows = append(windows, SegmentWindow{
				Rep:        rep.ID,
				Index:      i,
				StartUS:    prev,
				EndUS:      b,
				IsFinal:    i == len(boundaries)-1,
				FrameCount: frameCount(prev, b, rep.FrameDurationUS),
				GridSkewUS: skew,
				EdgeTrimUS: trimAt(i == len(boundaries)-1, trim),
			})
			prev = b
		}
	}
	return windows, maxSkew, nil
}

func trimAt(final bool, trim int64) int64 {
	if final {
		return trim
	}
	return 0
}

func frameCount(start, end, frameUS int64) int {
	if frameUS <= 0 || end <= start {
		return 0
	}
	return int((end - start + frameUS - 1) / frameUS)
}

func nearestSkew(sortedEdges []int64, b int64) int64 {
	if len(sortedEdges) == 0 {
		return 0
	}
	i := sort.Search(len(sortedEdges), func(i int) bool { return sortedEdges[i] >= b })
	best := int64(1<<62 - 1)
	consider := func(v int64) {
		d := v - b
		if d < 0 {
			d = -d
		}
		if d < best {
			best = d
		}
	}
	if i < len(sortedEdges) {
		consider(sortedEdges[i])
	}
	if i > 0 {
		consider(sortedEdges[i-1])
	}
	return best
}

// BoundaryDelta returns the largest start-time difference between
// representations that share a segment index. Because all windows are cut on
// one grid this reflects only real misalignment, never rounding noise.
func BoundaryDelta(segments []model.Segment) int64 {
	byIndex := map[int]map[model.RepresentationID]int64{}
	for _, s := range segments {
		m := byIndex[s.Index]
		if m == nil {
			m = map[model.RepresentationID]int64{}
			byIndex[s.Index] = m
		}
		m[s.Rep] = s.StartUS
	}
	var max int64
	for _, m := range byIndex {
		var vals []int64
		for _, v := range m {
			vals = append(vals, v)
		}
		if len(vals) < 2 {
			continue
		}
		sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
		if d := vals[len(vals)-1] - vals[0]; d > max {
			max = d
		}
	}
	return max
}
