// Package fanin merges per-rung views onto the shared clock and cross-checks
// boundary times and keyframe identity across representation layers.
package fanin

import "fmt"

// Boundary is one rung's view of a keyframe boundary on the shared clock.
type Boundary struct {
	Rung string
	Seq  uint64 // keyframe identity (access-unit sequence number)
	Time int64  // unified media-clock time
}

// Mismatch describes one cross-layer boundary disagreement.
type Mismatch struct {
	Seq    uint64
	Reason string
}

func (m Mismatch) String() string { return fmt.Sprintf("seq=%d: %s", m.Seq, m.Reason) }

// Check verifies that every keyframe boundary observed in any rung appears
// in all rungs with the same identity and (within tol) the same time.
func Check(perRung map[string][]Boundary, tol int64) []Mismatch {
	var mism []Mismatch
	// Collect the union of boundary sequences.
	type obs struct {
		rung string
		time int64
	}
	seen := map[uint64][]obs{}
	rungs := make([]string, 0, len(perRung))
	for rung, bs := range perRung {
		rungs = append(rungs, rung)
		for _, b := range bs {
			seen[b.Seq] = append(seen[b.Seq], obs{rung, b.Time})
		}
	}
	for seq, observers := range seen {
		if len(observers) != len(rungs) {
			present := map[string]bool{}
			for _, o := range observers {
				present[o.rung] = true
			}
			for _, r := range rungs {
				if !present[r] {
					mism = append(mism, Mismatch{seq, fmt.Sprintf("keyframe missing in rung %s", r)})
				}
			}
			continue
		}
		base := observers[0]
		for _, o := range observers[1:] {
			d := o.time - base.time
			if d < 0 {
				d = -d
			}
			if d > tol {
				mism = append(mism, Mismatch{seq,
					fmt.Sprintf("boundary time differs between %s and %s by %d ticks", base.rung, o.rung, d)})
			}
		}
	}
	return mism
}
