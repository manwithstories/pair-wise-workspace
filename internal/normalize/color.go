package normalize

import (
	"fmt"
	"sort"

	"fastpost/internal/model"
)

// ColorDesc is the color signaling of one video representation. The zero
// value means "unspecified"; a fully specified signal carries all three
// HLS color identifiers.
type ColorDesc struct {
	Track    string
	Primaries     string // e.g. "bt709", "bt2020"
	Transfer      string // e.g. "smpte2084" for PQ
	Matrix        string
	VideoRange    string // SDR, HLG, PQ (HLS VIDEO-RANGE)
}

// signature is the comparable identity of a color description. Tracks
// with identical signatures can share a representation; differing
// signatures must either be split into separate groups or rejected.
func (c ColorDesc) signature() string {
	return c.Primaries + "|" + c.Transfer + "|" + c.Matrix + "|" + c.VideoRange
}

// ColorGroup collects tracks that share one exact color description.
type ColorGroup struct {
	ID     string
	Desc   ColorDesc
	Tracks []string
}

// ColorPolicy chooses how to handle conflicting color metadata.
type ColorPolicy string

const (
	// ColorGroupOnConflict keeps every distinct description as its own
	// rendition group instead of forcing a merge.
	ColorGroupOnConflict ColorPolicy = "group"
	// ColorRejectOnConflict aborts publication when more than one distinct
	// description exists.
	ColorRejectOnConflict ColorPolicy = "reject"
)

// GroupColor partitions tracks by exact color signature. Under the reject
// policy a second distinct description is a hard fault; under the group
// policy each description becomes a stable, separately named group.
func GroupColor(descs []ColorDesc, policy ColorPolicy) ([]ColorGroup, error) {
	col := &model.Collector{}
	groups := map[string]*ColorGroup{}
	var order []string
	for _, d := range descs {
		sig := d.signature()
		g, ok := groups[sig]
		if !ok {
			if policy == ColorRejectOnConflict && len(order) > 0 {
				conflict := order[0]
				col.Add(model.FaultColorConflict, d.Track, -1,
					fmt.Sprintf("color %q conflicts with existing group %q", sig, conflict))
				continue
			}
			g = &ColorGroup{ID: "color-" + sig, Desc: d}
			groups[sig] = g
			order = append(order, sig)
		}
		g.Tracks = append(g.Tracks, d.Track)
	}
	if col.Has() {
		return nil, col.Err()
	}
	out := make([]ColorGroup, 0, len(order))
	for _, sig := range order {
		out = append(out, *groups[sig])
	}
	// Stable ordering keeps manifests byte-identical across runs.
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Conflicts reports whether the descriptions contain more than one
// distinct color signature. Callers use this before publication when the
// active policy cannot be decided locally.
func Conflicts(descs []ColorDesc) bool {
	seen := map[string]bool{}
	for _, d := range descs {
		seen[d.signature()] = true
	}
	return len(seen) > 1
}
