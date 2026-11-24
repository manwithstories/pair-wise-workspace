// Package repair performs bounded, deterministic recovery for one
// representation at a time. A representation's recovery never mutates another
// layer's results, and every unrecoverable window is reported as missing
// rather than silently dropped.
package repair

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"mediaflow/internal/clock"
	"mediaflow/internal/cmaf"
)

// Status classifies a recovered fragment.
type Status string

const (
	StatusComplete  Status = "complete"
	StatusPartial   Status = "partial"
	StatusMissing   Status = "missing"
	StatusDuplicate Status = "duplicate"
	StatusSkipped   Status = "skipped"
)

// Options bounds recovery work and tunes inference.
type Options struct {
	MaxFragments int
	MaxSegments  int
	// NominalFrameUs is used to size a missing window when a fragment has no
	// usable timing at all.
	NominalFrameUs int64
	// MinGapUs is the smallest hole that is reported as missing media.
	MinGapUs int64
}

// DefaultOptions returns the recovery limits used by the pipeline.
func DefaultOptions() Options {
	return Options{
		MaxFragments:   4096,
		MaxSegments:    4096,
		NominalFrameUs: 33_333,
		MinGapUs:       1_000,
	}
}

// FragmentPlan is the recovered placement of one fragment for one track.
type FragmentPlan struct {
	Segment        int    `json:"segment"`
	Sequence       uint32 `json:"sequence"`
	TrackID        uint32 `json:"track_id"`
	StartUs        int64  `json:"start_us"`
	EndUs          int64  `json:"end_us"`
	Samples        int    `json:"samples"`
	DroppedSamples int    `json:"dropped_samples"`
	Status         Status `json:"status"`
	Offset         uint64 `json:"offset"`
	// Trusted is false when the start time had to be inferred (missing or
	// regressed tfdt); such entries are never presented as authoritative.
	Trusted bool `json:"trusted"`
	// DuplicateOf is the segment index that first carried this sequence.
	DuplicateOf int `json:"duplicate_of,omitempty"`
}

// Recovery is the deterministic outcome for one representation track.
type Recovery struct {
	TrackID        uint32               `json:"track_id"`
	Fragments      []FragmentPlan       `json:"fragments"`
	Missing        clock.Coverage       `json:"missing"`
	TotalFragments int                  `json:"total_fragments"`
	Recovered      int                  `json:"recovered"`
	Partial        int                  `json:"partial"`
	MissingCount   int                  `json:"missing_fragments"`
	Duplicates     int                  `json:"duplicates"`
	Untrusted      int                  `json:"untrusted_fragments"`
	Overflow       bool                 `json:"overflow"`
	Digest         string               `json:"digest"`
	Errors         []clock.LocatedError `json:"errors,omitempty"`
	Index          Index                `json:"-"`
	// RebuiltSegments counts segments whose index had to be recomputed from
	// their actual fragments because the incremental index was incomplete.
	RebuiltSegments int `json:"rebuilt_segments"`
}

// Boundary returns the trusted media extent of the recovery.
func (r Recovery) Boundary() (start, end int64, ok bool) {
	for _, f := range r.Fragments {
		if f.Status == StatusMissing || f.Status == StatusDuplicate {
			continue
		}
		if !ok || f.StartUs < start {
			start = f.StartUs
		}
		if !ok || f.EndUs > end {
			end = f.EndUs
		}
		ok = true
	}
	return start, end, ok
}

// RecoverTrack recovers one track from scanned media segments. Samples are
// placed on the shared timeline; truncated samples and unrecoverable gaps are
// reported as missing media.
func RecoverTrack(tl *clock.Timeline, trackID uint32, segs []cmaf.MediaSegment, opts Options) Recovery {
	if opts.MaxFragments <= 0 {
		opts = DefaultOptions()
	}
	rec := Recovery{TrackID: trackID}
	seenSeq := make(map[uint32]int)
	var prevEnd int64
	havePrev := false
	boundReached := false

	var addMissing func(start, end int64, kind clock.ErrorKind, seg int, off uint64, trusted bool, detail string)
	addMissing = func(start, end int64, kind clock.ErrorKind, seg int, off uint64, trusted bool, detail string) {
		if end <= start {
			end = start + opts.NominalFrameUs
		}
		rec.Missing = clock.Merge(append(rec.Missing,
			clock.Interval{StartUs: start, EndUs: end}))
		rec.Errors = append(rec.Errors, clock.LocatedError{
			Kind: kind, AtUs: start, PositionKnown: trusted, TrackID: trackID,
			Offset: off, Segment: seg, Detail: detail,
		})
	}

	for si, seg := range segs {
		if si >= opts.MaxSegments {
			rec.Overflow = true
			break
		}
		for _, frag := range seg.Fragments {
			if len(rec.Fragments) >= opts.MaxFragments {
				rec.Overflow = true
				boundReached = true
				break
			}
			samples := trackSamples(frag, trackID)
			if len(samples) == 0 {
				continue
			}
			rec.TotalFragments++
			plan := FragmentPlan{
				Segment: seg.SegmentIndex, Sequence: frag.SequenceNumber, TrackID: trackID,
				Offset: frag.MoofOffset, Samples: len(samples),
			}
			// Duplicate detection is by mfhd sequence; a fragment without
			// mfhd cannot be deduplicated safely.
			if frag.HasMfhd {
				if firstSeg, dup := seenSeq[frag.SequenceNumber]; dup {
					rec.Duplicates++
					plan.Status = StatusDuplicate
					plan.DuplicateOf = firstSeg
					plan.Trusted = false
					plan.StartUs, plan.EndUs = sampleExtent(tl, samples, opts)
					rec.Fragments = append(rec.Fragments, plan)
					continue
				}
				seenSeq[frag.SequenceNumber] = seg.SegmentIndex
			}

			startUs, trusted := fragmentStart(tl, frag, trackID)
			if !trusted {
				// Missing tfdt: place after the previous fragment, but never
				// claim the offset is authoritative.
				if havePrev {
					startUs = prevEnd
				} else {
					startUs = sampleExtentStart(tl, samples)
				}
				rec.Errors = append(rec.Errors, clock.LocatedError{
					Kind: clock.ErrSegmentTruncated, AtUs: startUs, PositionKnown: havePrev,
					TrackID: trackID, Offset: frag.MoofOffset, Segment: seg.SegmentIndex,
					Detail: "fragment has no usable tfdt; start inferred from previous boundary",
				})
			}
			if havePrev && startUs < prevEnd {
				// tfdt regression: keep the error at the regressed position,
				// then re-anchor forward and mark the overlap as missing.
				rec.Errors = append(rec.Errors, clock.LocatedError{
					Kind: clock.ErrTfdtRegression, AtUs: startUs, PositionKnown: true,
					TrackID: trackID, Offset: frag.MoofOffset, Segment: seg.SegmentIndex,
					Detail: fmt.Sprintf("tfdt start %d us is below previous end %d us", startUs, prevEnd),
				})
				addMissing(startUs, prevEnd, clock.ErrTfdtRegression, seg.SegmentIndex, frag.MoofOffset, true,
					"tfdt overlap re-anchored")
				startUs = prevEnd
				trusted = false
			}
			if havePrev && startUs > prevEnd+opts.MinGapUs {
				addMissing(prevEnd, startUs, clock.ErrSegmentTruncated, seg.SegmentIndex, frag.MoofOffset, true,
					"gap before fragment")
			}

			plan.StartUs = startUs
			plan.Trusted = trusted
			baseTicks, hasBaseTicks := frag.TfdtTicks[trackID]
			tickCursor := baseTicks
			cursor := startUs
			for _, sm := range samples {
				// Convert every sample start from its tick position directly;
				// accumulating per-sample microsecond durations would drift
				// by a fraction of a tick per sample.
				sampleStart := cursor
				if hasBaseTicks {
					if us, err := tl.ToUs(trackID, tickCursor); err == nil {
						sampleStart = us
					}
				}
				dur := durationUs(tl, sm)
				tickCursor += uint64(sm.DurationTicks)
				if sm.Truncated || sm.Available < sm.Size {
					plan.DroppedSamples++
					addMissing(sampleStart, sampleStart+dur, clock.ErrSampleOutOfBounds, seg.SegmentIndex,
						sm.Offset, true, fmt.Sprintf("sample %d truncated", sm.Index))
				} else {
					plan.Samples++
				}
				cursor = sampleStart + dur
			}
			if hasBaseTicks {
				if us, err := tl.ToUs(trackID, tickCursor); err == nil {
					cursor = us
				}
			}
			plan.EndUs = cursor
			switch {
			case plan.DroppedSamples == 0:
				plan.Status = StatusComplete
				rec.Recovered++
			case plan.Samples > 0:
				plan.Status = StatusPartial
				rec.Partial++
			default:
				plan.Status = StatusMissing
				rec.MissingCount++
				addMissing(plan.StartUs, plan.EndUs, clock.ErrMissingRecovered, seg.SegmentIndex, frag.MoofOffset, trusted,
					"all samples in fragment are unusable")
			}
			if !trusted {
				rec.Untrusted++
			}
			rec.Fragments = append(rec.Fragments, plan)
			prevEnd = plan.EndUs
			havePrev = true
		}
		if boundReached {
			break
		}
	}

	rec.BoundaryGaps(opts)
	rec.Digest = digest(rec)
	rec.Index, rec.RebuiltSegments = RebuildIndex(tl, rec, segs)
	return rec
}

// BoundaryGaps fills unexplained holes inside the recovered span, so a
// fragment that never appeared in any segment is still reported as missing.
func (r *Recovery) BoundaryGaps(opts Options) {
	start, end, ok := r.Boundary()
	if !ok {
		return
	}
	window := clock.Interval{StartUs: start, EndUs: end}
	gaps := clock.Coverage(r.covered()).Gaps(window, opts.MinGapUs)
	if len(gaps) > 0 {
		r.Missing = clock.Merge(append(r.Missing, gaps...))
	}
}

// covered returns the recoverable spans of the plans.
func (r Recovery) covered() []clock.Interval {
	var ivs []clock.Interval
	for _, f := range r.Fragments {
		if f.Status == StatusDuplicate || f.Status == StatusMissing {
			continue
		}
		if f.EndUs > f.StartUs {
			ivs = append(ivs, clock.Interval{StartUs: f.StartUs, EndUs: f.EndUs})
		}
	}
	return ivs
}

func digest(r Recovery) string {
	h := sha256.New()
	plans := make([]FragmentPlan, len(r.Fragments))
	copy(plans, r.Fragments)
	sort.SliceStable(plans, func(i, j int) bool {
		if plans[i].Segment != plans[j].Segment {
			return plans[i].Segment < plans[j].Segment
		}
		if plans[i].Sequence != plans[j].Sequence {
			return plans[i].Sequence < plans[j].Sequence
		}
		return plans[i].StartUs < plans[j].StartUs
	})
	for _, p := range plans {
		fmt.Fprintf(h, "%d|%d|%d|%d|%d|%d|%d|%s|%v\n",
			p.TrackID, p.Segment, p.Sequence, p.StartUs, p.EndUs, p.Samples, p.DroppedSamples, p.Status, p.Trusted)
	}
	for _, m := range r.Missing {
		fmt.Fprintf(h, "missing|%d|%d|%d\n", r.TrackID, m.StartUs, m.EndUs)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func trackSamples(frag cmaf.Fragment, trackID uint32) []cmaf.Sample {
	out := make([]cmaf.Sample, 0, len(frag.Samples))
	for _, sm := range frag.Samples {
		if sm.TrackID == trackID {
			out = append(out, sm)
		}
	}
	return out
}

func fragmentStart(tl *clock.Timeline, frag cmaf.Fragment, trackID uint32) (int64, bool) {
	base, ok := frag.TfdtTicks[trackID]
	if !ok {
		return 0, false
	}
	us, err := tl.ToUs(trackID, base)
	if err != nil {
		return 0, false
	}
	return us, true
}

func sampleExtentStart(tl *clock.Timeline, samples []cmaf.Sample) int64 {
	us, err := tl.ToUs(samples[0].TrackID, samples[0].DecodeTimeTicks)
	if err != nil {
		return 0
	}
	return us
}

func sampleExtent(tl *clock.Timeline, samples []cmaf.Sample, opts Options) (int64, int64) {
	start := sampleExtentStart(tl, samples)
	end := start
	for _, sm := range samples {
		end += durationUs(tl, sm)
	}
	if end <= start {
		end = start + opts.NominalFrameUs
	}
	return start, end
}

func durationUs(tl *clock.Timeline, sm cmaf.Sample) int64 {
	tc, ok := tl.Track(sm.TrackID)
	if !ok || tc.Timescale == 0 {
		return 33_333
	}
	us, err := tc.ToUnifiedUs(uint64(sm.DurationTicks))
	if err != nil {
		return 33_333
	}
	// ToUnifiedUs anchors at the track origin; durations are differences, so
	// subtract the zero-tick conversion to get the span.
	zero, _ := tc.ToUnifiedUs(0)
	d := us - zero
	if d <= 0 {
		return 33_333
	}
	return d
}
