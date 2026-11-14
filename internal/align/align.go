package align

import (
	"fmt"
	"sort"
	"sync"

	"segmanifest/internal/ladder"
	"segmanifest/internal/manifest"
)

type repResult struct {
	rep    manifest.Representation
	disc   []manifest.Discontinuity
	resync []manifest.ResyncPoint
	stats  manifest.RepresentationStats
}

// Align snaps every representation boundary to the shared grid. Drift inside
// tolerance is compensated directly. A run of three over-tolerance boundaries
// in the same direction creates a resynchronization point; every such boundary
// is also reported as a discontinuity. Segment numbers are never renumbered.
func Align(m *manifest.Manifest, grid ladder.Grid) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if len(grid.Points) < 2 || grid.TargetUS <= 0 || grid.ToleranceUS <= 0 {
		return manifest.NewError("align.invalid_grid", "grid must have at least two points")
	}
	if grid.Points[0] != 0 {
		return manifest.NewError("align.invalid_grid", "grid must start at zero")
	}
	results := make([]repResult, len(m.Representations))
	errs := make([]error, len(m.Representations))
	var wg sync.WaitGroup
	for i := range m.Representations {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			result, err := alignRepresentation(m.Representations[index], grid)
			if err != nil {
				errs[index] = err
				return
			}
			results[index] = result
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			return fmt.Errorf("representation %s: %w", m.Representations[i].ID, err)
		}
	}
	m.Representations = make([]manifest.Representation, len(results))
	m.Discontinuities = m.Discontinuities[:0]
	m.ResyncPoints = m.ResyncPoints[:0]
	m.Stats = make([]manifest.RepresentationStats, len(results))
	for i, result := range results {
		m.Representations[i] = result.rep
		m.Discontinuities = append(m.Discontinuities, result.disc...)
		m.ResyncPoints = append(m.ResyncPoints, result.resync...)
		m.Stats[i] = result.stats
	}
	return m.Validate()
}

func alignRepresentation(rep manifest.Representation, grid ladder.Grid) (repResult, error) {
	result := repResult{rep: rep}
	if len(rep.Segments) == 0 {
		return result, manifest.NewError("align.empty_representation", "representation %q has no segments", rep.ID)
	}
	raw := make([]int64, len(rep.Segments)+1)
	for i := range rep.Segments {
		if i > 0 && rep.Segments[i].StartUS <= rep.Segments[i-1].StartUS {
			return result, manifest.NewError("align.non_monotonic_start", "segment %d does not advance", rep.Segments[i].Number)
		}
		raw[i] = rep.Segments[i].StartUS
	}
	last := rep.Segments[len(rep.Segments)-1]
	if last.StartUS > (1<<62)-last.DurationUS {
		return result, manifest.NewError("align.duration_overflow", "segment %d end overflows", last.Number)
	}
	raw[len(raw)-1] = last.StartUS + last.DurationUS
	if raw[len(raw)-1] > grid.Points[len(grid.Points)-1] {
		return result, manifest.NewError("align.end_outside_grid", "timeline ends after the last grid point")
	}

	indexes := make([]int, len(raw))
	drifts := make([]int64, len(raw))
	previousIndex := 0
	for i, rawTime := range raw {
		index, drift := grid.Nearest(rawTime)
		if index < previousIndex {
			index = previousIndex
			drift = rawTime - grid.Points[index]
		}
		indexes[i], drifts[i], previousIndex = index, drift, index
	}
	firstDuration := rep.Segments[0].DurationUS
	truncated := rep.AudioOffsetUS < 0 && firstDuration > -rep.AudioOffsetUS
	if raw[0] < 0 && raw[0]+firstDuration > 0 {
		truncated = true
	}
	run, runDirection := 0, 0
	var samples []int64
	for i := range rep.Segments {
		segment := &rep.Segments[i]
		segment.RawStartUS = raw[i]
		segment.StartUS = grid.Points[indexes[i]]
		segment.DriftUS = drifts[i]
		segment.DurationUS = grid.Points[indexes[i+1]] - grid.Points[indexes[i]]
		segment.Truncated = i == 0 && truncated
		if segment.DurationUS <= 0 {
			return result, manifest.NewError("align.invalid_duration", "segment %d collapses on the grid", segment.Number)
		}
		absDrift := abs(segment.DriftUS)
		samples = append(samples, absDrift)
		if absDrift > grid.ToleranceUS {
			direction := sign(segment.DriftUS)
			segment.Discontinuity = true
			result.disc = append(result.disc, manifest.Discontinuity{
				RepresentationID: rep.ID,
				SegmentNumber:    segment.Number,
				RawStartUS:       segment.RawStartUS,
				GridStartUS:      segment.StartUS,
				DriftUS:          segment.DriftUS,
				Direction:        direction,
			})
			if direction == runDirection {
				run++
			} else {
				run, runDirection = 1, direction
			}
			if run == 3 {
				result.resync = append(result.resync, manifest.ResyncPoint{
					RepresentationID: rep.ID,
					SegmentNumber:    segment.Number,
					GridStartUS:      segment.StartUS,
					DriftUS:          segment.DriftUS,
					Direction:        direction,
				})
				run = 0
				runDirection = 0
			}
		} else {
			run, runDirection = 0, 0
		}
	}
	result.rep = rep
	result.stats = makeStats(rep, samples, len(result.resync))
	return result, nil
}

func makeStats(rep manifest.Representation, samples []int64, resyncPoints int) manifest.RepresentationStats {
	stats := manifest.RepresentationStats{
		RepresentationID: rep.ID,
		Bandwidth:        rep.Bandwidth,
		Samples:          len(samples),
		ResyncPoints:     resyncPoints,
	}
	for _, value := range samples {
		stats.TotalAbsUS += value
		if value > stats.MaxDriftUS {
			stats.MaxDriftUS = value
		}
	}
	ordered := append([]int64(nil), samples...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	if len(ordered) > 0 {
		rank := (95*len(ordered) + 99) / 100
		if rank < 1 {
			rank = 1
		}
		stats.P95DriftUS = ordered[rank-1]
	}
	return stats
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func sign(v int64) int {
	if v < 0 {
		return -1
	}
	if v > 0 {
		return 1
	}
	return 0
}
