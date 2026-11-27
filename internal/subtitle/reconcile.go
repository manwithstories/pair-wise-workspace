package subtitle

import (
	"avrepair/internal/clock"
)

// ReconcileResult is the output of a subtitle reconciliation pass.
type ReconcileResult struct {
	Cues     []Cue // non-overlapping, strictly ordered cues
	Isolated []Cue // cues that could not be repaired
	Errors   []*clock.TrackError
	Repairs  int
}

// Reconcile enforces a sane subtitle timeline on a single track:
//
//   - start timestamps are fed through the clock monotonicity repair, so
//     rollbacks and duplicates share the exact 500 ms rule as media tracks;
//   - a cue whose end overlaps the next cue's start is trimmed to the next
//     start minus one nanosecond and flagged CodeSubtitleOverlap;
//   - a zero- or negative-duration cue after trimming is isolated.
//
// The input is never modified.
func Reconcile(tr *Track) ReconcileResult {
	res := ReconcileResult{}
	if tr == nil || len(tr.Cues) == 0 {
		return res
	}
	// Reuse the shared clock repair for start-time rollback/duplicate handling.
	pts := make([]clock.Point, len(tr.Cues))
	for i, c := range tr.Cues {
		pts[i] = clock.Point{Track: tr.Name, Seq: c.Index, TS: c.StartNS}
	}
	tl := clock.RepairTimeline(pts)
	res.Errors = append(res.Errors, tl.Errors...)
	res.Repairs += tl.Repairs

	bySeq := map[int]Cue{}
	for _, c := range tr.Cues {
		bySeq[c.Index] = c
	}
	for _, p := range tl.Repaired {
		c := bySeq[p.Seq]
		c.StartNS = p.TS
		res.Cues = append(res.Cues, c)
	}
	for _, p := range tl.Isolated {
		res.Isolated = append(res.Isolated, bySeq[p.Seq])
	}

	// A repaired start can meet or pass the original end. Clamp the end to at
	// least start+1; cues with no room are isolated rather than emitted.
	kept := res.Cues[:0]
	for _, c := range res.Cues {
		if c.EndNS <= c.StartNS {
			res.Errors = append(res.Errors, clock.NewError(clock.CodeSubtitleOverlap,
				tr.Name, clock.Interval{Start: c.StartNS, End: c.EndNS},
				"non-positive duration; isolating").
				WithAction(clock.ActionIsolated))
			res.Isolated = append(res.Isolated, c)
			continue
		}
		kept = append(kept, c)
	}
	res.Cues = kept

	// Resolve end overlaps against the repaired, ordered start timeline.
	out := res.Cues[:0]
	for i := range res.Cues {
		cur := res.Cues[i]
		if i+1 < len(res.Cues) && cur.EndNS > res.Cues[i+1].StartNS {
			res.Errors = append(res.Errors, clock.NewError(clock.CodeSubtitleOverlap,
				tr.Name, clock.Interval{Start: res.Cues[i+1].StartNS, End: cur.EndNS},
				"cue end overlaps next cue start; trimming").
				WithAction(clock.ActionTrimmed))
			cur.EndNS = res.Cues[i+1].StartNS - 1
			res.Repairs++
		}
		if cur.EndNS <= cur.StartNS {
			res.Errors = append(res.Errors, clock.NewError(clock.CodeSubtitleOverlap,
				tr.Name, clock.Interval{Start: cur.StartNS, End: cur.EndNS},
				"non-positive duration after trim; isolating").
				WithAction(clock.ActionIsolated))
			res.Isolated = append(res.Isolated, cur)
			continue
		}
		out = append(out, cur)
	}
	res.Cues = out
	return res
}

// OverlapCount is a lightweight predicate used by fixtures and tests.
func OverlapCount(errs []*clock.TrackError) int {
	n := 0
	for _, e := range errs {
		if e.Code == clock.CodeSubtitleOverlap {
			n++
		}
	}
	return n
}
