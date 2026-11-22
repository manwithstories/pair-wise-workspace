package manifest

import (
	"mediapack/internal/clock"
)

// InputEvent is the minimal event shape the aligner consumes; the
// timeline engine's Event is adapted to it by the pipeline.
type InputEvent struct {
	Track      uint64
	TimeNS     int64
	ByteOff    int64
	PayloadLen int64
	Keyframe   bool
}

// GridSpec configures the common alignment grid and the tolerance used
// when a representation's boundary does not land exactly on a cell edge.
type GridSpec struct {
	GridNS       int64
	ToleranceNS  int64
	Reps         []Representation
}

// Align partitions every input event into grid cells per representation
// and builds one Segment per (representation, cell). Representations
// share identical time boundaries (the common grid); per-cell problems
// are collected as StructuredError values instead of aborting the run.
func Align(events []InputEvent, spec GridSpec, fingerprint string) (*Manifest, error) {
	if spec.GridNS <= 0 {
		return nil, clock.NewError(clock.CatAlignment, "grid must be positive, got %d", spec.GridNS)
	}
	if len(spec.Reps) < 2 {
		return nil, clock.NewError(clock.CatAlignment, "need at least two representations, got %d", len(spec.Reps))
	}
	m := &Manifest{
		SourceFingerprint: fingerprint,
		GridNS:            spec.GridNS,
		Representations:   append([]Representation(nil), spec.Reps...),
	}

	// Determine the full time span across all events; an empty input
	// still yields one empty cell so manifests remain well-formed.
	var minT, maxT int64
	have := false
	for _, ev := range events {
		if !have || ev.TimeNS < minT {
			minT = ev.TimeNS
		}
		if !have || ev.TimeNS > maxT {
			maxT = ev.TimeNS
		}
		have = true
	}
	var firstCell, lastCell int64
	if !have {
		m.Errors = append(m.Errors, StructuredError{
			Kind: "empty_segment", Detail: "no input events; emitting single empty cell",
		})
		// One cell spanning [0, GridNS): first and last cell coincide.
		firstCell, lastCell = int64(0), int64(0)
	} else {
		firstCell = floorGrid(minT, spec.GridNS)
		lastCell = floorGrid(maxT, spec.GridNS)
	}

	// Index events by track for fast representation membership tests.
	byTrack := make(map[uint64][]InputEvent)
	for _, ev := range events {
		byTrack[ev.Track] = append(byTrack[ev.Track], ev)
	}

	idx := 0
	for cellStart := firstCell; cellStart <= lastCell; cellStart += spec.GridNS {
		cellEnd := cellStart + spec.GridNS
		for _, rep := range spec.Reps {
			seg := Segment{
				Index: idx, StartNS: cellStart, EndNS: cellEnd,
				Representation: rep.ID, Tracks: sortedCopy(rep.Tracks),
			}
			seg = m.buildCell(seg, rep, byTrack, spec)
			m.Segments = append(m.Segments, seg)
			idx++
		}
	}
	return m, nil
}

// buildCell fills one representation's cell and records structured
// errors for missing tracks, empty cells and off-grid boundaries.
func (m *Manifest) buildCell(seg Segment, rep Representation,
	byTrack map[uint64][]InputEvent, spec GridSpec) Segment {
	for _, tr := range rep.Tracks {
		evs, ok := byTrack[tr]
		if !ok {
			m.Errors = append(m.Errors, StructuredError{
				Kind: "missing_track", Rep: rep.ID, Track: tr,
				StartNS: seg.StartNS, Detail: "track has no events anywhere in source",
			})
			continue
		}
		for _, ev := range evs {
			if ev.TimeNS < seg.StartNS || ev.TimeNS >= seg.EndNS {
				continue
			}
			seg.Blocks++
			seg.Bytes += ev.PayloadLen
			// Boundary tolerance: keyframe events near a cell edge must
			// sit within tolerance of the grid, else the representation's
			// boundary cannot be matched to the common timeline.
			d := distanceToGrid(ev.TimeNS, spec.GridNS)
			if d > spec.ToleranceNS && ev.Keyframe {
				m.Errors = append(m.Errors, StructuredError{
					Kind: "unmatched_boundary", Rep: rep.ID, Track: tr,
					StartNS: seg.StartNS,
					Detail: "keyframe boundary off grid beyond tolerance",
				})
			}
		}
	}
	if seg.Blocks == 0 {
		seg.Empty = true
		m.Errors = append(m.Errors, StructuredError{
			Kind: "empty_segment", Rep: rep.ID, StartNS: seg.StartNS,
			Detail: "no blocks intersect this grid cell",
		})
	}
	return seg
}

func floorGrid(t, grid int64) int64 {
	if t >= 0 {
		return (t / grid) * grid
	}
	return ((t - grid + 1) / grid) * grid
}

// distanceToGrid returns the absolute distance from t to the nearest
// grid boundary.
func distanceToGrid(t, grid int64) int64 {
	rem := t % grid
	if rem < 0 {
		rem += grid
	}
	if d := grid - rem; d < rem {
		return d
	}
	return rem
}
