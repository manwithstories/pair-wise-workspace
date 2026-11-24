package timeline

import (
	"math"
	"sort"

	"mediad/internal/core"
)

// spanTicks sums sample durations with overflow checking.
func spanTicks(o Observation) (int64, bool) {
	var total int64
	for _, s := range o.Samples {
		nt, ok := core.AddChecked(total, int64(s.Duration))
		if !ok {
			return total, false
		}
		total = nt
	}
	return total, true
}

// firstCTS returns the composition offset of the first sample (keyframe).
func firstCTS(o Observation) int64 {
	if len(o.Samples) > 0 {
		return int64(o.Samples[0].CTOffset)
	}
	return 0
}

// Repair deduplicates by content digest, records sequence anomalies, adjudicates
// tfdt rollback with RTP evidence and converts starts to the 90 kHz basis.
func Repair(in []Observation) *Result {
	res := &Result{Periods: 1}

	// digest dedup; deterministic keeper = smallest path.
	byDigest := map[string]Observation{}
	var unique []Observation
	for _, o := range in {
		if prev, ok := byDigest[o.Digest]; ok {
			keep, drop := prev.Path, o.Path
			if o.Path < prev.Path {
				keep, drop = o.Path, prev.Path
				byDigest[o.Digest] = o
			}
			res.Duplicates = append(res.Duplicates, Duplicate{Digest: o.Digest, Kept: keep, Dropped: drop})
			res.addIssue(IssueDuplicateDigest, o.Representation, int64(o.Sequence), "duplicate fragment digest collapsed")
			continue
		}
		byDigest[o.Digest] = o
		unique = append(unique, o)
	}
	sort.SliceStable(res.Duplicates, func(i, j int) bool { return res.Duplicates[i].Dropped < res.Duplicates[j].Dropped })

	groups := map[string][]Observation{}
	var repOrder []string
	for _, o := range unique {
		if _, ok := groups[o.Representation]; !ok {
			repOrder = append(repOrder, o.Representation)
		}
		groups[o.Representation] = append(groups[o.Representation], o)
	}
	sort.Strings(repOrder)

	for _, rep := range repOrder {
		items := groups[rep]
		recordOrderAnomalies(items, rep, res)
		sort.SliceStable(items, func(i, j int) bool {
			if items[i].Sequence != items[j].Sequence {
				return items[i].Sequence < items[j].Sequence
			}
			return items[i].Digest < items[j].Digest
		})
		repairGroup(items, rep, res)
	}

	res.Fragments = CanonicalOrder(res.Fragments)
	return res
}

// recordOrderAnomalies inspects discovery/path order for out-of-order and
// duplicate sequence numbers; gaps are measured on the sorted sequence set.
func recordOrderAnomalies(items []Observation, rep string, res *Result) {
	seen := map[uint32]bool{}
	var maxSeq int64 = -1
	for _, o := range items {
		if !o.HasSequence {
			continue
		}
		s := int64(o.Sequence)
		if seen[o.Sequence] {
			res.addIssue(IssueDuplicateSequence, rep, s, "mfhd sequence number repeated")
		}
		seen[o.Sequence] = true
		if maxSeq >= 0 && s < maxSeq {
			res.addIssue(IssueOutOfOrder, rep, s, "fragment sequence arrived before a later sequence")
		}
		if s > maxSeq {
			maxSeq = s
		}
	}
	seqs := make([]int, 0, len(seen))
	for s := range seen {
		seqs = append(seqs, int(s))
	}
	sort.Ints(seqs)
	for i := 1; i < len(seqs); i++ {
		if miss := seqs[i] - seqs[i-1] - 1; miss > 0 {
			res.Issues = append(res.Issues, Issue{
				Code: IssueGap, Representation: rep, Sequence: int64(seqs[i]), Missing: miss,
				Message: "missing sequence fragment(s)",
			})
		}
	}
}

func repairGroup(items []Observation, rep string, res *Result) {
	period := 0
	var prev *Fragment
	var prevRTP RTPEvidence
	var prevDuration int64

	for _, o := range items {
		f := &Fragment{Observation: o, Period: period, TimingValid: true}
		dur, ok := spanTicks(o)
		f.DurationTicks = dur
		if !ok {
			f.TimingValid = false
			res.addIssue(IssueOverflow, rep, int64(o.Sequence), "sample duration sum overflow")
		}
		base, bok := tfdtInt64(o.TFDT)
		if !bok {
			f.TimingValid = false
			res.addIssue(IssueOverflow, rep, int64(o.Sequence), "tfdt exceeds int64")
		}

		if prev != nil && bok && base < prev.StartDTS {
			res.addIssue(IssueTFDTRegression, rep, int64(o.Sequence), "tfdt moved backwards")
			if rtpContinuity(prev.Observation.RTP, prevDuration, o.RTP, o.Timescale) {
				// Corroborated continuous capture clock: trust RTP and extend.
				f.StartDTS = prev.EndDTS
				f.RTPRepaired = true
				res.addIssue(IssueRTPContinuityRepair, rep, int64(o.Sequence),
					"RTP timestamps corroborate continuity; tfdt repaired to previous end")
			} else {
				period++
				f.Period = period
				code := IssueNewPeriodMissingRTP
				if o.RTP.Present && prevRTP.Present {
					code = IssueNewPeriodRTPConflict
				}
				res.addIssue(code, rep, int64(o.Sequence),
					"opening explicit new period; timeline continuity is not fabricated")
				f.StartDTS = base
			}
		} else if bok {
			f.StartDTS = base
		}

		if f.TimingValid && bok {
			end, ok := core.AddChecked(f.StartDTS, dur)
			if !ok {
				f.TimingValid = false
				res.addIssue(IssueOverflow, rep, int64(o.Sequence), "DTS end overflow")
				f.EndDTS = math.MaxInt64
			} else {
				f.EndDTS = end
			}
			pts, ok := core.AddChecked(f.StartDTS, firstCTS(o))
			if !ok {
				f.TimingValid = false
				res.addIssue(IssueOverflow, rep, int64(o.Sequence), "PTS composition overflow")
			} else if o.Timescale == 0 {
				res.addIssue(IssueBadTimescale, rep, int64(o.Sequence), "zero timescale")
			} else if p90, ok := core.ScaleTicksFloor(pts, o.Timescale, CommonTimescale); !ok {
				f.TimingValid = false
				res.addIssue(IssueOverflow, rep, int64(o.Sequence), "PTS rational conversion overflow")
			} else {
				f.StartPTS90k = p90
			}
			checkDTSMonotonic(o, f, res, rep)
		}

		res.Fragments = append(res.Fragments, f)
		prev = f
		prevRTP = o.RTP
		prevDuration = dur
	}
	if period+1 > res.Periods {
		res.Periods = period + 1
	}
}

func checkDTSMonotonic(o Observation, f *Fragment, res *Result, rep string) {
	prev := f.StartDTS
	cursor := f.StartDTS
	for i, s := range o.Samples {
		if i > 0 {
			next, ok := core.AddChecked(cursor, int64(s.Duration))
			if !ok {
				f.TimingValid = false
				res.addIssue(IssueOverflow, rep, int64(o.Sequence), "DTS monotonic overflow")
				return
			}
			cursor = next
			if cursor < prev {
				// Unreachable for unsigned durations absent overflow, but the
				// invariant is checked explicitly rather than assumed.
				f.TimingValid = false
				res.addIssue(IssueOverflow, rep, int64(o.Sequence), "DTS moved backwards within trun")
				return
			}
			prev = cursor
		}
	}
}

func tfdtInt64(v uint64) (int64, bool) {
	if v > math.MaxInt64 {
		return 0, false
	}
	return int64(v), true
}

// CanonicalOrder sorts by period, presentation time, representation, sequence
// and finally digest so merge output is scheduling-independent.
func CanonicalOrder(in []*Fragment) []*Fragment {
	out := append([]*Fragment(nil), in...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Period != b.Period {
			return a.Period < b.Period
		}
		if a.StartPTS90k != b.StartPTS90k {
			return a.StartPTS90k < b.StartPTS90k
		}
		if a.Representation != b.Representation {
			return a.Representation < b.Representation
		}
		if a.Sequence != b.Sequence {
			return a.Sequence < b.Sequence
		}
		return a.Digest < b.Digest
	})
	return out
}

// rtpContinuity reports whether RTP deltas corroborate a continuous capture
// clock across the boundary. The allowed error is one nominal frame interval.
func rtpContinuity(prevRTP RTPEvidence, prevDurTicks int64, cur RTPEvidence, timescale uint32) bool {
	if !prevRTP.Present || !cur.Present || prevRTP.ClockRate == 0 || cur.ClockRate != prevRTP.ClockRate {
		return false
	}
	delta := int64(int32(cur.Timestamp - prevRTP.Timestamp)) // 32-bit wrap safe
	if delta <= 0 {
		return false
	}
	expected, ok := core.ScaleTicksFloor(prevDurTicks, timescale, prevRTP.ClockRate)
	if !ok || expected <= 0 {
		return false
	}
	tol, ok := core.MulDivFloor(int64(prevRTP.ClockRate), 1, 30) // nominal 30fps frame
	if !ok || tol < 1 {
		tol = 1
	}
	d := delta - expected
	if d < 0 {
		d = -d
	}
	return d <= tol
}
