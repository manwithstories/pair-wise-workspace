package pipeline

import (
	"fmt"

	"avrepair/internal/graph"
	"avrepair/internal/media"
)

// repairTracks normalizes every track onto a shared base, unwraps 33-bit
// clocks, removes duplicated timestamps, aligns audio to video and writes
// repaired segment outputs. It returns a deterministic result hash.
func (c *runContext) repairTracks() (string, error) {
	tl := c.timeline
	if tl == nil {
		return "", fmt.Errorf("pipeline: no timeline to repair")
	}
	if err := c.planAndCheck(); err != nil {
		return "", err
	}
	// Unwrap each track relative to its own previous raw value.
	for _, track := range tl.Tracks {
		if err := c.unwrapTrack(track); err != nil {
			return "", err
		}
	}
	base := firstPTS(tl)
	tl.Base = base
	for _, track := range tl.Tracks {
		track.Origin = base
		shiftTrack(track, base.Neg())
	}
	if err := c.alignAudio(tl); err != nil {
		return "", err
	}
	for _, track := range tl.Tracks {
		c.dedupeTrack(track)
	}
	// Outputs are written before quarantine records are collected so a
	// segment that fails during materialization also produces a reason.
	if err := c.writeOutputs(tl); err != nil {
		return "", err
	}
	c.collectQuarantine(tl)
	c.report.AVSyncMaxMS = avSyncMax(tl).Millis()
	c.report.Segments = countSegments(tl, nil)
	c.report.SegmentsValid = countSegments(tl, func(s *media.Segment) bool { return s.Valid() })
	c.report.Quarantined = len(tl.Quarantine)
	c.report.Corrections = len(tl.Corrections)
	c.report.P95SegmentMS = percentile(c.segTimes, 0.95)
	return c.hashTimeline(tl), nil
}

func (c *runContext) planAndCheck() error {
	p, err := c.plan()
	if err != nil {
		return err
	}
	return p.Validate()
}

// unwrapTrack resolves 33-bit wrap for TS tracks. Ambiguous deltas and
// non-increasing sample clocks quarantine the segment.
func (c *runContext) unwrapTrack(track *media.Track) error {
	var prev int64
	have := false
	for _, seg := range track.Segments {
		if seg.Quarantine != "" {
			continue
		}
		if seg.Format == "ts" || seg.Format == "ts-capture" {
			for i := range seg.Samples {
				s := &seg.Samples[i]
				if !have {
					prev = s.Raw
					have = true
					continue
				}
				un, ok := media.Unwrap33(prev, s.Raw)
				if !ok {
					seg.QuarantineWith(media.ReasonAmbiguousWrap,
						fmt.Sprintf("delta exactly half of 33-bit range at sample %d", i))
					break
				}
				s.Adjusted = un - s.Raw
				s.Raw = un
				s.PTS = media.Ticks(un, s.Scale)
				prev = un
			}
		}
	}
	return nil
}

func firstPTS(tl *media.Timeline) media.Rational {
	best := media.Zero
	have := false
	for _, t := range tl.Tracks {
		for _, s := range t.Segments {
			if len(s.Samples) == 0 {
				continue
			}
			p := s.Samples[0].PTS
			if !have || p.Cmp(best) < 0 {
				best = p
				have = true
			}
		}
	}
	return best
}

func shiftTrack(track *media.Track, delta media.Rational) {
	for _, seg := range track.Segments {
		for i := range seg.Samples {
			seg.Samples[i].PTS = seg.Samples[i].PTS.Add(delta)
		}
		if len(seg.Samples) > 0 {
			seg.DeclaredStart = seg.Samples[0].PTS
		}
	}
}

// alignAudio computes the median audio-video offset per paired segment
// index and shifts the audio timeline to match the first video track.
func (c *runContext) alignAudio(tl *media.Timeline) error {
	var ref *media.Track
	var aud *media.Track
	for _, t := range tl.Tracks {
		if ref == nil && t.Kind == media.TrackVideo {
			ref = t
		}
		if aud == nil && t.Kind == media.TrackAudio {
			aud = t
		}
	}
	if ref == nil || aud == nil {
		return nil
	}
	// Pair by declared sequence rather than slice position so quarantined
	// or reordered entries do not misalign the two tracks.
	var a, v []media.Rational
	for _, vs := range ref.Segments {
		as := findSegmentBySeq(aud, vs.Seq)
		if as == nil || len(as.Samples) == 0 || len(vs.Samples) == 0 {
			continue
		}
		a = append(a, as.Samples[0].PTS)
		v = append(v, vs.Samples[0].PTS)
	}
	off, ok := media.MedianOffset(a, v)
	if !ok {
		return nil
	}
	shift := off.Neg()
	aud.Shift = shift
	shiftTrack(aud, shift)
	tl.Corrections = append(tl.Corrections, media.Correction{
		Track: aud.ID, Kind: "av_offset", Before: off.String(), After: "0",
		Detail: fmt.Sprintf("audio shifted by %dus", shift.MillisRounded()*1000),
	})
	return nil
}

// dedupeTrack enforces strict monotonicity. Boundary repeats are shifted
// forward by one sample period; interior duplicates are dropped when the
// choice is unambiguous, otherwise the segment is quarantined.
func (c *runContext) dedupeTrack(track *media.Track) {
	var last *media.Rational
	for _, seg := range track.Segments {
		if seg.Quarantine != "" || len(seg.Samples) == 0 {
			continue
		}
		if last != nil && seg.Samples[0].PTS.Cmp(*last) <= 0 {
			step := segStep(seg)
			if step.Cmp(media.Zero) <= 0 {
				seg.QuarantineWith(media.ReasonDuplicateUnfix, "no usable sample period")
				continue
			}
			delta := last.Add(step).Sub(seg.Samples[0].PTS)
			before := seg.Samples[0].PTS
			for i := range seg.Samples {
				seg.Samples[i].PTS = seg.Samples[i].PTS.Add(delta)
			}
			track.Shift = track.Shift.Add(delta)
			c.timeline.Corrections = append(c.timeline.Corrections, media.Correction{
				Segment: seg.ID, Track: track.ID, Kind: "boundary_duplicate",
				Before: before.String(), After: seg.Samples[0].PTS.String(),
				Detail: "shifted forward by one sample period",
			})
		}
		last = &seg.Samples[len(seg.Samples)-1].PTS
		if !strictlyIncreasing(seg.Samples) {
			dropInteriorDuplicates(c.timeline, track, seg)
			if len(seg.Samples) == 0 {
				seg.QuarantineWith(media.ReasonDuplicateUnfix, "all samples were duplicates")
				continue
			}
			last = &seg.Samples[len(seg.Samples)-1].PTS
		}
		seg.Duration = durationOf(seg)
	}
	// Enforce monotonicity across the whole track after adjustments.
	if !trackIncreasing(track) {
		for _, seg := range track.Segments {
			if seg.Valid() {
				seg.QuarantineWith(media.ReasonDuplicateUnfix, "track clock remains non-monotonic")
			}
		}
	}
}

func dropInteriorDuplicates(tl *media.Timeline, track *media.Track, seg *media.Segment) {
	kept := seg.Samples[:0]
	var prev *media.Rational
	dup := 0
	for _, s := range seg.Samples {
		if prev != nil && s.PTS.Cmp(*prev) <= 0 {
			dup++
			continue
		}
		kept = append(kept, s)
		p := s.PTS
		prev = &p
	}
	seg.Samples = kept
	if dup > 0 {
		tl.Corrections = append(tl.Corrections, media.Correction{
			Segment: seg.ID, Track: track.ID, Kind: "interior_duplicate",
			Before: fmt.Sprintf("%d samples", len(kept)+dup),
			After:  fmt.Sprintf("%d samples", len(kept)),
			Detail: fmt.Sprintf("dropped %d non-increasing samples", dup),
		})
	}
}

func (c *runContext) plan() (*graph.Plan, error) {
	var videos, audios []string
	for _, t := range c.timeline.Tracks {
		switch t.Kind {
		case media.TrackAudio:
			audios = append(audios, t.ID)
		default:
			videos = append(videos, t.ID)
		}
	}
	return graph.Build(videos, audios)
}
