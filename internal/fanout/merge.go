package fanout

import (
	"fmt"

	"tscheck/internal/model"
)

// MergeReport summarizes the cross-branch boundary check.
type MergeReport struct {
	MaxDelta  model.Ticks // largest keyframe delta across branches
	Conflicts []string
}

// MergeBoundaries verifies that all branches recovered to the same
// keyframe boundaries on the shared clock. Corresponding keyframes must
// sit within one frame of each other; larger deltas are conflicts.
func MergeBoundaries(results []*BranchResult) MergeReport {
	rep := MergeReport{}
	if len(results) < 2 {
		return rep
	}
	base := results[0]
	for _, other := range results[1:] {
		a, b := base.GOP.KeyframeTicks, other.GOP.KeyframeTicks
		if len(a) != len(b) {
			rep.Conflicts = append(rep.Conflicts, fmt.Sprintf(
				"%s vs %s: keyframe count %d != %d",
				base.Name, other.Name, len(a), len(b)))
			continue
		}
		for i := range a {
			d := a[i] - b[i]
			if d < 0 {
				d = -d
			}
			if d > rep.MaxDelta {
				rep.MaxDelta = d
			}
			if d > model.FrameTicks {
				rep.Conflicts = append(rep.Conflicts, fmt.Sprintf(
					"%s vs %s: keyframe %d delta %d ticks exceeds one frame",
					base.Name, other.Name, i, d))
			}
		}
	}
	return rep
}
