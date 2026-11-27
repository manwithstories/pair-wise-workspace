package clock

// Point is one timestamped observation on a track. Seq preserves the original
// ordinal so repaired and isolated outputs stay traceable to the input.
type Point struct {
	Track string
	Seq   int
	TS    int64
	Tag   string
}

// RepairResult is the output of a monotonicity repair pass.
type RepairResult struct {
	Repaired []Point       // strictly increasing timestamps
	Isolated []Point       // points whose correction exceeded MaxRepairDelta
	Errors   []*TrackError // structured defects, in input order
	Repairs  int           // count of applied nudges/shifts
}

// RepairTimeline enforces a strictly increasing nanosecond timeline.
//
//   - A timestamp greater than the previous accepted one passes through.
//   - A duplicate is nudged forward by one nanosecond.
//   - A rollback is shifted to previous+1 when the required correction is at
//     most MaxRepairDelta (500 ms); otherwise the offending point is isolated
//     and the accepted timeline is left untouched.
//
// The input slice is never modified.
func RepairTimeline(points []Point) RepairResult {
	res := RepairResult{}
	var lastTS int64
	have := false
	for _, p := range points {
		if !have || p.TS > lastTS {
			res.Repaired = append(res.Repaired, p)
			lastTS = p.TS
			have = true
			continue
		}
		want := lastTS + 1
		delta := want - p.TS // always positive here
		if delta > MaxRepairDelta {
			err := NewError(CodeRollback, p.Track,
				Interval{Start: p.TS, End: want},
				"rollback exceeds 500ms repair limit").WithAction(ActionIsolated)
			res.Errors = append(res.Errors, err)
			res.Isolated = append(res.Isolated, p)
			continue
		}
		code := CodeRollback
		action := ActionShifted
		if p.TS == lastTS {
			code = CodeDuplicateTS
			action = ActionNudged
		}
		fixed := p
		fixed.TS = want
		res.Repaired = append(res.Repaired, fixed)
		res.Errors = append(res.Errors, NewError(code, p.Track,
			Interval{Start: p.TS, End: want},
			"non-increasing timestamp corrected").WithAction(action))
		lastTS = want
		res.Repairs++
	}
	return res
}

// MergePoints interleaves per-track repaired points into one strictly
// increasing global timeline. Equal timestamps keep input order and are nudged
// by one nanosecond; points are expected to already be monotonic per track.
func MergePoints(streams ...[]Point) []Point {
	var all []Point
	for _, s := range streams {
		all = append(all, s...)
	}
	// Stable ordering by (timestamp, original ordinal) keeps output
	// deterministic across reruns.
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && all[j-1].TS > all[j].TS; j-- {
			all[j-1], all[j] = all[j], all[j-1]
		}
	}
	var last int64
	for i := range all {
		if i > 0 && all[i].TS <= last {
			all[i].TS = last + 1
		}
		last = all[i].TS
	}
	return all
}

// CheckMonotonic validates that timestamps are strictly increasing; it returns
// the first violation as a structured error.
func CheckMonotonic(track string, ts []int64) error {
	for i := 1; i < len(ts); i++ {
		if ts[i] <= ts[i-1] {
			return NewError(CodeNonMonotonic, track,
				Interval{Start: ts[i-1], End: ts[i]},
				"timestamps not strictly increasing")
		}
	}
	return nil
}
