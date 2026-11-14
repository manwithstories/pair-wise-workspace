package ladder

import (
	"sort"

	"segmanifest/internal/manifest"
)

type Rung struct {
	ID        string
	Bandwidth uint64
}

// NormalizeLadder sorts rungs by bandwidth and removes duplicate bandwidths.
// Duplicate representation IDs remain an error because they cannot be addressed
// unambiguously by emit and templates.
func NormalizeLadder(rungs []Rung) ([]Rung, error) {
	seenIDs := make(map[string]struct{}, len(rungs))
	for _, r := range rungs {
		if r.ID == "" {
			return nil, manifest.NewError("ladder.empty_id", "representation ID is empty")
		}
		if r.Bandwidth == 0 {
			return nil, manifest.NewError("ladder.zero_bandwidth", "representation %q has zero bandwidth", r.ID)
		}
		if _, ok := seenIDs[r.ID]; ok {
			return nil, manifest.NewError("ladder.duplicate_id", "duplicate representation ID %q", r.ID)
		}
		seenIDs[r.ID] = struct{}{}
	}
	sorted := append([]Rung(nil), rungs...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Bandwidth != sorted[j].Bandwidth {
			return sorted[i].Bandwidth < sorted[j].Bandwidth
		}
		return sorted[i].ID < sorted[j].ID
	})
	out := sorted[:0]
	for _, r := range sorted {
		if len(out) > 0 && out[len(out)-1].Bandwidth == r.Bandwidth {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

type Grid struct {
	TargetUS    int64
	ToleranceUS int64
	Points      []int64
}

func ValidateConfig(targetUS, toleranceUS int64) error {
	if targetUS <= 0 {
		return manifest.NewError("ladder.invalid_target", "target duration must be positive")
	}
	if toleranceUS <= 0 || toleranceUS > targetUS/2 {
		return manifest.NewError("ladder.invalid_tolerance", "tolerance must be greater than zero and at most half the target duration")
	}
	return nil
}

func BuildGrid(reps []manifest.Representation, targetUS, toleranceUS int64) (Grid, error) {
	if err := ValidateConfig(targetUS, toleranceUS); err != nil {
		return Grid{}, err
	}
	if len(reps) == 0 {
		return Grid{}, manifest.NewError("ladder.no_representations", "at least one representation is required")
	}
	var maxEnd int64
	for _, rep := range reps {
		var end int64
		for _, seg := range rep.Segments {
			if seg.DurationUS <= 0 {
				return Grid{}, manifest.NewError("ladder.invalid_duration", "representation %q segment %d has non-positive duration", rep.ID, seg.Number)
			}
			if seg.StartUS > (1<<62)-seg.DurationUS {
				return Grid{}, manifest.NewError("ladder.duration_overflow", "representation %q duration overflows", rep.ID)
			}
			if segmentEnd := seg.StartUS + seg.DurationUS; segmentEnd > end {
				end = segmentEnd
			}
		}
		if end > maxEnd {
			maxEnd = end
		}
	}
	if maxEnd <= 0 {
		return Grid{}, manifest.NewError("ladder.empty_timeline", "representations have no timed segments")
	}
	count := maxEnd / targetUS
	if maxEnd%targetUS != 0 {
		count++
	}
	points := make([]int64, count+1)
	for i := range points {
		points[i] = int64(i) * targetUS
	}
	return Grid{TargetUS: targetUS, ToleranceUS: toleranceUS, Points: points}, nil
}

func (g Grid) Nearest(timeUS int64) (index int, driftUS int64) {
	if len(g.Points) == 0 {
		return 0, timeUS
	}
	i := sort.Search(len(g.Points), func(i int) bool { return g.Points[i] >= timeUS })
	if i == 0 {
		return 0, timeUS - g.Points[0]
	}
	if i == len(g.Points) {
		i--
		return i, timeUS - g.Points[i]
	}
	if timeUS-g.Points[i-1] <= g.Points[i]-timeUS {
		return i - 1, timeUS - g.Points[i-1]
	}
	return i, timeUS - g.Points[i]
}
