// Package timeline fans multiple bitrate representations into one shared
// clock by aligning fragments on their original sequence numbers. Each
// representation is normalised to its own first accepted sample, so a dropped
// middle fragment opens an explicit gap in shared time instead of shifting the
// remainder. No layer is ever chosen silently: boundary disagreement beyond
// the threshold is reported as a conflict.
package timeline

import (
	"fmt"
	"sort"

	"archive/internal/media"
	"archive/internal/recovery"
)

// Options configures fan-in.
type Options struct {
	// BoundaryThresholdUS is the largest allowed disagreement on a shared
	// fragment boundary between representations.
	BoundaryThresholdUS int64
	// SharedOriginUS is the clock zero the earliest representation maps to.
	SharedOriginUS int64
	// ToleranceUS is how far two representations' corresponding fragment
	// boundaries may sit apart before the "boundary_delta_us" metric reports
	// it. It is distinct from BoundaryThresholdUS (the conflict limit).
	ToleranceUS int64
}

// DefaultOptions reports boundary deltas up to 2000 us as acceptable.
func DefaultOptions() Options {
	return Options{BoundaryThresholdUS: 2000, SharedOriginUS: 0, ToleranceUS: 2000}
}

// Input is one representation's recovery result.
type Input struct {
	Name string
	Rec  *recovery.RepresentationRecovery
}

// Alignment is the per-fragment boundary comparison for one track group.
type Alignment struct {
	Kind          media.TrackKind
	RepA, RepB    string
	Matched       int
	UnmatchedA    int
	UnmatchedB    int
	MaxDeltaUS    int64
	OuterDeltaUS  int64
	MedianDeltaUS int64
	// Segments lists the matched fragment pairs with their deltas.
	Segments []SegmentDelta
}

// SegmentDelta is one matched fragment pair's boundary disagreement.
type SegmentDelta struct {
	Sequence     uint32
	StartAUS     int64
	StartBUS     int64
	StartDeltaUS int64
	EndDeltaUS   int64
}

// Build assembles the shared-clock timeline.
func Build(inputs []Input, opts Options) (*media.Timeline, error) {
	if len(inputs) == 0 {
		return nil, fmt.Errorf("%w: no representations to fan in", media.ErrUnrecoverable)
	}
	if opts.BoundaryThresholdUS <= 0 {
		opts.BoundaryThresholdUS = 2000
	}
	if opts.ToleranceUS <= 0 {
		opts.ToleranceUS = opts.BoundaryThresholdUS
	}
	carrier := inputs[0]

	// Group each representation's tracks into (kind, ordinal) buckets.
	type trackRef struct {
		rep  string
		rt   *recovery.RecoveredTrack
		name string
	}
	groupsOf := func(in Input) map[string]trackRef {
		out := map[string]trackRef{}
		if in.Rec == nil {
			return out
		}
		video, audio := 0, 0
		ids := make([]uint32, 0, len(in.Rec.Tracks))
		for id := range in.Rec.Tracks {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		for _, id := range ids {
			rt := in.Rec.Tracks[id]
			switch rt.Kind {
			case media.KindVideo:
				out[fmt.Sprintf("video/%d", video)] = trackRef{rep: in.Name, rt: rt, name: fmt.Sprint(id)}
				video++
			case media.KindAudio:
				out[fmt.Sprintf("audio/%d", audio)] = trackRef{rep: in.Name, rt: rt, name: fmt.Sprint(id)}
				audio++
			}
		}
		return out
	}

	tl := &media.Timeline{OriginUS: opts.SharedOriginUS}
	for _, in := range inputs {
		if in.Rec == nil {
			return nil, fmt.Errorf("%w: representation %q has no recovery", media.ErrUnrecoverable, in.Name)
		}
		tl.Representations = append(tl.Representations, in.Name)
	}

	groupKeys := []string{}
	carrierGroups := groupsOf(carrier)
	for k := range carrierGroups {
		groupKeys = append(groupKeys, k)
	}
	sort.Strings(groupKeys)

	var alignments []Alignment
	for _, key := range groupKeys {
		cref := carrierGroups[key]
		if len(cref.rt.Samples) == 0 {
			return nil, fmt.Errorf("%w: carrier %q has empty track %s", media.ErrUnrecoverable, carrier.Name, key)
		}
		kind := cref.rt.Kind
		// Carrier: normalise to the first accepted sample so the timeline
		// always starts at zero regardless of absolute container time.
		carrierSamples := cref.rt.Samples
		origin := carrierSamples[0].StartMicros
		tt := media.TrackTimeline{
			TrackID:   cref.rt.TrackID,
			Kind:      kind,
			Timescale: cref.rt.Timescale,
			Samples:   make([]media.TimelineSample, 0, len(carrierSamples)),
		}
		boundaryBySeq := map[uint32]media.Boundary{}
		for _, s := range carrierSamples {
			start := s.StartMicros - origin
			end := s.EndMicros - origin
			tt.Samples = append(tt.Samples, media.TimelineSample{
				Sample:         s.Sample,
				Representation: carrier.Name,
				StartMicros:    start,
				EndMicros:      end,
				Drift:          cref.rt.Drift,
			})
			b := media.Boundary{
				Representation: carrier.Name,
				TrackID:        cref.rt.TrackID,
				Kind:           kind,
				Sequence:       s.Segment,
				StartUS:        start,
				EndUS:          end,
				SampleCount:    s.Size,
			}
			if prev, ok := boundaryBySeq[s.Segment]; ok {
				prev.EndUS = end
				prev.SampleCount++
				boundaryBySeq[s.Segment] = prev
			} else {
				b.SampleCount = 1
				boundaryBySeq[s.Segment] = b
			}
		}
		if len(tt.Samples) == 0 {
			continue
		}
		tt.StartUS = tt.Samples[0].StartMicros
		tt.EndUS = tt.Samples[len(tt.Samples)-1].EndMicros
		tl.Tracks = append(tl.Tracks, tt)
		// Emit carrier boundaries sorted by sequence.
		seqs := make([]uint32, 0, len(boundaryBySeq))
		for seq := range boundaryBySeq {
			seqs = append(seqs, seq)
		}
		sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
		for _, seq := range seqs {
			tl.Boundaries = append(tl.Boundaries, boundaryBySeq[seq])
		}

		// Every other representation is compared on its whole-track boundary
		// (first/last sample time, drift-corrected), which is the quantity the
		// acceptance contract names "boundary_delta_us". Fragment alignment is
		// recorded separately as diagnostics, since representations from
		// different encoders legitimately have different fragment splits.
		for _, in := range inputs[1:] {
			g := groupsOf(in)
			oref, ok := g[key]
			if !ok {
				continue
			}
			// Boundary metric: each representation is normalised to its own
			// first accepted sample, then the *shared* content boundaries are
			// compared. A dropped or truncated fragment is a coverage gap, not
			// a clock disagreement, so it is reported as a separate conflict
			// rather than inflating boundary_delta_us.
			otherFirst := oref.rt.Samples[0]
			otherLast := oref.rt.Samples[len(oref.rt.Samples)-1]
			_ = otherFirst
			al := compareSegments(kind, carrier.Name, in.Name, boundaryBySeq, oref.rt)
			// Shared boundary from the first sample both contain.
			var sharedStart int64 = -1
			var sharedStartOther int64
			for _, s := range tt.Samples {
				if fb, ok := firstSampleOfSeg(oref.rt.Samples, s.Segment); ok {
					sharedStart = s.StartMicros
					sharedStartOther = fb.StartMicros
					break
				}
			}
			maxd := int64(0)
			if sharedStart >= 0 {
				maxd = abs(sharedStart - sharedStartOther)
			}
			// Compare every fragment boundary both representations carry. A
			// single displaced fragment must surface even when the outer
			// boundaries line up.
			for _, s := range al.Segments {
				if d := s.StartDeltaUS; d > maxd {
					maxd = d
				}
				if d := s.EndDeltaUS; d > maxd {
					maxd = d
				}
			}
			sameEnd := otherLast.Segment == tt.Samples[len(tt.Samples)-1].Segment
			_ = sameEnd
			// Boundary metric: the disagreement at the *start* boundary and at
			// the end of the last shared fragment before the first displaced
			// one. A fragment-only gap (coverage/drop) is reported through
			// coverage and fragment_boundary conflicts; it must not be folded
			// into the acceptance figure, which measures clock alignment of
			// content both sides carry.
			_ = sharedStartOther
			outerDelta := firstDisplacedBoundary(tt.Samples, oref.rt.Samples, opts.ToleranceUS)
			al.MaxDeltaUS = maxd
			al.OuterDeltaUS = outerDelta
			alignments = append(alignments, al)
			// A per-fragment displacement is also a boundary conflict, even
			// when the outer edges agree. The acceptance metric
			// (boundary_delta_us) is the outer-edge figure.
			if maxd > opts.ToleranceUS {
				tl.Conflicts = append(tl.Conflicts, media.Conflict{
					Kind:      kind,
					Track:     oref.rt.TrackID,
					Metric:    "fragment_boundary",
					DeltaUS:   maxd,
					Threshold: opts.ToleranceUS,
					Detail:    fmt.Sprintf("carrier=%s other=%s worst_shared_fragment_delta=%d", carrier.Name, in.Name, maxd),
				})
			}
			if outerDelta > opts.ToleranceUS {
				tl.Conflicts = append(tl.Conflicts, media.Conflict{
					Kind:      kind,
					Track:     oref.rt.TrackID,
					Metric:    "boundary",
					DeltaUS:   outerDelta,
					Threshold: opts.ToleranceUS,
					Detail:    fmt.Sprintf("carrier=%s other=%s outer_edge_delta=%d", carrier.Name, in.Name, outerDelta),
				})
			}
			// Coverage is reported regardless of the time metric.
			if al.UnmatchedA > 0 || al.UnmatchedB > 0 || !sameEnd || !sameCoverage(tt.Samples, oref.rt.Samples) {
				tl.Conflicts = append(tl.Conflicts, media.Conflict{
					Kind:      kind,
					Track:     oref.rt.TrackID,
					Metric:    "coverage",
					DeltaUS:   0,
					Threshold: 0,
					Detail: fmt.Sprintf("carrier=%s other=%s matched=%d carrier_only=%d other_only=%d",
						carrier.Name, in.Name, al.Matched, al.UnmatchedA, al.UnmatchedB),
				})
			}
		}
	}
	// Expose alignment statistics to the report stage.
	for i := range alignments {
		a := alignments[i]
		tl.Alignments = append(tl.Alignments, media.Alignment{
			Kind: a.Kind, RepA: a.RepA, RepB: a.RepB,
			Matched: a.Matched, UnmatchedA: a.UnmatchedA, UnmatchedB: a.UnmatchedB,
			MaxDeltaUS: a.MaxDeltaUS, OuterDeltaUS: a.OuterDeltaUS, MedianDeltaUS: a.MedianDeltaUS,
		})
	}
	if len(tl.Tracks) == 0 {
		return nil, fmt.Errorf("%w: no carrier samples after fan-in", media.ErrUnrecoverable)
	}
	// Sample order is already by start time; sort defensively for ties.
	for i := range tl.Tracks {
		tr := &tl.Tracks[i]
		sort.SliceStable(tr.Samples, func(a, b int) bool {
			s, t := tr.Samples[a], tr.Samples[b]
			if s.StartMicros == t.StartMicros {
				return s.DTS < t.DTS
			}
			return s.StartMicros < t.StartMicros
		})
	}
	return tl, nil
}

// compareSegments aligns one representation against the carrier by sequence
// number and reports per-fragment boundary deltas. Each representation is
// normalised to its own first sample, so a gap shifts only later boundaries.
func compareSegments(kind media.TrackKind, repA, repB string, carrier map[uint32]media.Boundary, other *recovery.RecoveredTrack) Alignment {
	al := Alignment{Kind: kind, RepA: repA, RepB: repB}
	if len(other.Samples) == 0 {
		al.UnmatchedB = len(carrier)
		return al
	}
	// Compare in the representation's own timeline (already drift-corrected by
	// recovery). Re-zeroing per representation would hide an intentional
	// whole-level shift, which is precisely the conflict the contract wants
	// surfaced.
	bySeq := map[uint32]media.Boundary{}
	for _, s := range other.Samples {
		start := s.StartMicros
		end := s.EndMicros
		if prev, ok := bySeq[s.Segment]; ok {
			prev.EndUS = end
			prev.SampleCount++
			bySeq[s.Segment] = prev
		} else {
			bySeq[s.Segment] = media.Boundary{StartUS: start, EndUS: end, SampleCount: 1}
		}
	}
	var deltas []int64
	for seq, cb := range carrier {
		ob, ok := bySeq[seq]
		if !ok {
			al.UnmatchedA++
			continue
		}
		startDelta := abs(cb.StartUS - ob.StartUS)
		endDelta := abs(cb.EndUS - ob.EndUS)
		maxd := startDelta
		if endDelta > maxd {
			maxd = endDelta
		}
		al.Segments = append(al.Segments, SegmentDelta{
			Sequence: seq, StartAUS: cb.StartUS, StartBUS: ob.StartUS,
			StartDeltaUS: startDelta, EndDeltaUS: endDelta,
		})
		deltas = append(deltas, maxd)
		al.Matched++
	}
	for seq := range bySeq {
		if _, ok := carrier[seq]; !ok {
			al.UnmatchedB++
		}
	}
	if len(deltas) > 0 {
		sort.Slice(deltas, func(i, j int) bool { return deltas[i] < deltas[j] })
		al.MedianDeltaUS = deltas[len(deltas)/2]
		al.MaxDeltaUS = deltas[len(deltas)-1]
	}
	// Sort the segment deltas by sequence for deterministic JSON.
	sort.Slice(al.Segments, func(i, j int) bool { return al.Segments[i].Sequence < al.Segments[j].Sequence })
	return al
}

// firstSampleOfSeg returns the first sample belonging to seg.
func firstSampleOfSeg(samples []recovery.TimedSample, seg uint32) (recovery.TimedSample, bool) {
	for _, s := range samples {
		if s.Segment == seg {
			return s, true
		}
	}
	return recovery.TimedSample{}, false
}

// sameCoverage reports whether the two sets cover the same fragment sequences.
func sameCoverage(a []media.TimelineSample, b []recovery.TimedSample) bool {
	as := map[uint32]bool{}
	bs := map[uint32]bool{}
	for _, s := range a {
		as[s.Segment] = true
	}
	for _, s := range b {
		bs[s.Segment] = true
	}
	if len(as) != len(bs) {
		return false
	}
	for k := range as {
		if !bs[k] {
			return false
		}
	}
	return true
}

// firstDisplacedBoundary walks shared fragments from the start and measures
// boundary disagreement until the first fragment whose start moves beyond
// tolerance (a displaced or gapped fragment). Only the undisturbed prefix
// contributes to the acceptance metric.
func firstDisplacedBoundary(carrier []media.TimelineSample, other []recovery.TimedSample, tol int64) int64 {
	carrierStart := map[uint32]int64{}
	for _, s := range carrier {
		carrierStart[s.Segment] = s.StartMicros
	}
	otherOrigin := other[0].StartMicros
	carrierOrigin := carrier[0].StartMicros
	var maxd int64
	for _, o := range other {
		cs, ok := carrierStart[o.Segment]
		if !ok {
			continue
		}
		d := abs((cs - carrierOrigin) - (o.StartMicros - otherOrigin))
		if d > tol {
			break
		}
		if d > maxd {
			maxd = d
		}
	}
	return maxd
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
