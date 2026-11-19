// Package metrics aggregates demux, segmentation and alignment results into
// the manifest and renders the terminal report: target vs actual bitrate
// deviation, sample quality statistics, missing intervals and per-representation
// time alignment error.
package metrics

import (
	"fmt"
	"io"
	"sort"

	"mediapack/internal/clock"
	"mediapack/internal/demux"
	"mediapack/internal/ladder"
	"mediapack/internal/manifest"
	"mediapack/internal/segment"
)

// Input carries everything needed to assemble a manifest.
type Input struct {
	Container           *demux.Container
	Bounds              []segment.Boundary
	Alignment           *ladder.Alignment
	TargetBitrates      []uint64
	SegmentTargetMs     int64
	ToleranceMs         int64
	CheckpointRecovered bool
}

// Assemble builds the manifest from processing results. Video tracks are
// sorted by ascending average bitrate and paired with the sorted target
// bitrates; a count mismatch is an error. Segment boundary timestamps are
// additionally validated through an RTP timestamp round-trip.
func Assemble(in Input) (*manifest.Manifest, error) {
	c := in.Container
	reps := c.VideoTracks()
	if len(reps) == 0 {
		return nil, fmt.Errorf("no video track found")
	}
	sort.Slice(reps, func(i, j int) bool { return avgBitrate(reps[i]) < avgBitrate(reps[j]) })
	targets := append([]uint64(nil), in.TargetBitrates...)
	sort.Slice(targets, func(i, j int) bool { return targets[i] < targets[j] })
	// No targets given: each representation is its own baseline and the
	// deviation reports zero. Otherwise the counts must match.
	if len(targets) > 0 && len(targets) != len(reps) {
		return nil, fmt.Errorf("target bitrate count %d does not match video track count %d",
			len(targets), len(reps))
	}
	var boundaryTicks []int64
	for _, b := range in.Bounds {
		boundaryTicks = append(boundaryTicks, b.Ticks)
	}
	if err := clock.CheckRTPSequence(boundaryTicks, 90000, 96); err != nil {
		return nil, fmt.Errorf("rtp timestamp validation: %w", err)
	}
	m := &manifest.Manifest{
		Version:             1,
		Input:               c.Path,
		Fingerprint:         c.Fingerprint,
		SegmentTargetMs:     in.SegmentTargetMs,
		ToleranceMs:         in.ToleranceMs,
		CheckpointRecovered: in.CheckpointRecovered,
	}
	master := reps[len(reps)-1]
	end := clock.Ticks(master.Duration(), master.Timescale)
	for i, b := range in.Bounds {
		segEnd := end
		if i+1 < len(in.Bounds) {
			segEnd = in.Bounds[i+1].Ticks
		}
		m.Segments = append(m.Segments, manifest.SegmentEntry{
			Index: i, StartMs: clock.Millis(b.Ticks), EndMs: clock.Millis(segEnd),
			KeyframeSample: b.SampleIndex,
		})
	}
	for i, rep := range reps {
		actual := avgBitrate(rep)
		target := actual
		if len(targets) > 0 {
			target = targets[i]
		}
		dev := 0.0
		if target > 0 {
			dev = float64(int64(actual)-int64(target)) / float64(target) * 100
		}
		ra := repAlignment(in.Alignment, rep.ID)
		m.Representations = append(m.Representations, manifest.Representation{
			ID: fmt.Sprintf("rep%d", i), TrackID: rep.ID,
			TargetBitrate: target, ActualBitrate: actual,
			BitrateDeviationPct: dev,
			MaxAlignmentErrorMs: clock.Millis(ra.MaxErrorTicks),
			Aligned:             ra.Aligned,
		})
	}
	if in.Alignment != nil {
		m.MaxAlignmentErrorMs = clock.Millis(in.Alignment.MaxErrorTicks)
	}
	m.Quality = quality(c)
	m.MissingIntervals = intervals(append(append([]demux.Interval{}, c.Missing...), alignmentMissing(in.Alignment)...))
	m.UntrustedIntervals = intervals(c.Untrusted)
	return m, nil
}

type repAlignView struct {
	MaxErrorTicks int64
	Aligned       bool
}

func repAlignment(a *ladder.Alignment, trackID uint32) repAlignView {
	if a == nil {
		return repAlignView{Aligned: true}
	}
	for _, ra := range a.Reps {
		if ra.TrackID == trackID {
			v := repAlignView{Aligned: true}
			for i, e := range ra.ErrTicks {
				if !ra.Aligned[i] {
					v.Aligned = false
				}
				if e < 0 {
					e = -e
				}
				if e > v.MaxErrorTicks {
					v.MaxErrorTicks = e
				}
			}
			return v
		}
	}
	return repAlignView{Aligned: true}
}

// avgBitrate computes the mean bitrate of a track over its own duration.
func avgBitrate(t *demux.Track) uint64 {
	var bytes uint64
	for _, s := range t.Samples {
		if s.Available {
			bytes += uint64(s.Size)
		}
	}
	sec := clock.Seconds(clock.Ticks(t.Duration(), t.Timescale))
	if sec <= 0 {
		return 0
	}
	return uint64(float64(bytes) * 8 / sec)
}

// quality tallies measured, skipped and unmeasurable samples.
func quality(c *demux.Container) manifest.Quality {
	var q manifest.Quality
	for _, t := range c.Tracks {
		for _, s := range t.Samples {
			if s.Available {
				q.Samples++
				q.Bytes += uint64(s.Size)
			} else {
				q.Skipped++
			}
			if inIntervals(clock.Ticks(s.PTS, t.Timescale), c.Untrusted) {
				q.Unmeasurable++
			}
		}
	}
	return q
}

func inIntervals(ticks int64, ivs []demux.Interval) bool {
	for _, iv := range ivs {
		if ticks >= iv.StartTicks && ticks < iv.EndTicks {
			return true
		}
	}
	return false
}

func alignmentMissing(a *ladder.Alignment) []demux.Interval {
	if a == nil {
		return nil
	}
	return a.Missing
}

func intervals(ivs []demux.Interval) []manifest.Interval {
	var out []manifest.Interval
	for _, iv := range ivs {
		out = append(out, manifest.Interval{
			StartMs: clock.Millis(iv.StartTicks), EndMs: clock.Millis(iv.EndTicks),
			Reason: iv.Reason,
		})
	}
	return out
}

// Render writes the terminal report with the required keys.
func Render(w io.Writer, m *manifest.Manifest) {
	fmt.Fprintf(w, "segments: %d\n", len(m.Segments))
	fmt.Fprintf(w, "representations: %d\n", len(m.Representations))
	for _, r := range m.Representations {
		fmt.Fprintf(w, "  %s track=%d target_bitrate=%d actual_bitrate=%d bitrate_deviation_pct=%.2f max_alignment_error_ms=%.2f aligned=%v\n",
			r.ID, r.TrackID, r.TargetBitrate, r.ActualBitrate,
			r.BitrateDeviationPct, r.MaxAlignmentErrorMs, r.Aligned)
	}
	fmt.Fprintf(w, "quality_samples: %d\n", m.Quality.Samples)
	fmt.Fprintf(w, "quality_skipped: %d\n", m.Quality.Skipped)
	fmt.Fprintf(w, "quality_unmeasurable: %d\n", m.Quality.Unmeasurable)
	fmt.Fprintf(w, "max_alignment_error_ms: %.2f\n", m.MaxAlignmentErrorMs)
	fmt.Fprintf(w, "missing_intervals: %d\n", len(m.MissingIntervals))
	for _, iv := range m.MissingIntervals {
		fmt.Fprintf(w, "  %.0fms..%.0fms %s\n", iv.StartMs, iv.EndMs, iv.Reason)
	}
	fmt.Fprintf(w, "checkpoint_recovered: %v\n", m.CheckpointRecovered)
	fmt.Fprintf(w, "manifest_digest: %s\n", m.Digest)
}
