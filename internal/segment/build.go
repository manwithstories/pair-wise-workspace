// Package segment derives playlist boundaries on keyframe-aligned cuts and
// reconciles them across representations of the same source.
package segment

import (
	"fmt"
	"sort"

	"archive.local/recover/internal/errs"
	"archive.local/recover/internal/model"
)

// Config controls boundary selection.
type Config struct {
	// TargetNs is the nominal segment duration.
	TargetNs int64
	// MinNs and MaxNs bound any single segment. Zero means 0.5x / 1.5x target.
	MinNs int64
	MaxNs int64
	// MaxBoundaryNs is the largest acceptable distance from a target cut to the
	// keyframe that actually carries it.
	MaxBoundaryNs int64
	// KeyframeToleranceNs allows a keyframe slightly off the target to count as
	// an exact hit for multi-rendition alignment.
	KeyframeToleranceNs int64
}

func (c Config) withDefaults() Config {
	if c.TargetNs <= 0 {
		c.TargetNs = 4_000_000_000
	}
	if c.MinNs <= 0 {
		c.MinNs = c.TargetNs / 2
	}
	if c.MaxNs <= 0 {
		c.MaxNs = c.TargetNs * 3 / 2
	}
	if c.MaxBoundaryNs <= 0 {
		c.MaxBoundaryNs = 40_000_000
	}
	if c.KeyframeToleranceNs <= 0 {
		c.KeyframeToleranceNs = 40_000_000
	}
	return c
}

// Cut is one chosen segment boundary.
type Cut struct {
	Index   int   `json:"index"`
	StartNs int64 `json:"start_ns"`
	EndNs   int64 `json:"end_ns"`
	// KeyframeSample is the sample index that starts the segment; it must be a
	// keyframe on every representation that shares the cut.
	KeyframeSample int `json:"keyframe_sample"`
	// TargetNs is the unconstrained ideal boundary; DeviationNs is how far the
	// actual keyframe sits from it.
	TargetNs    int64 `json:"target_ns"`
	DeviationNs int64 `json:"deviation_ns"`
	// Pending marks a segment whose boundary could not meet the tolerance.
	Pending bool `json:"pending_review"`
}

// Result is a single rendition's segmentation.
type Result struct {
	Cuts           []Cut     `json:"cuts"`
	SegmentCount   int       `json:"segments"`
	MaxDeviationNs int64     `json:"max_boundary_deviation_ns"`
	Pending        int       `json:"pending_review"`
	Issues         errs.List `json:"issues"`
}

// Build cuts the timeline at keyframe boundaries nearest each target.
//
// keyframes must be sorted indexes into tr.PTS and every keyframe index must be
// in range; the caller validated that already.
func Build(tr *model.TrackTimeline, keyframes []int, cfg Config) *Result {
	cfg = cfg.withDefaults()
	res := &Result{}
	loc := func(sample int) errs.Location {
		return errs.Location{TrackID: tr.TrackID, Kind: tr.Kind, Sample: sample, Offset: -1}
	}
	if len(tr.PTS) == 0 {
		res.Issues.AddFatal(errs.ClassDependency, errs.CodeDependencyMissing,
			"segmenting a track with no samples", loc(-1))
		return res
	}
	if len(keyframes) == 0 {
		res.Issues.AddFatal(errs.ClassDependency, errs.CodeDependencyMissing,
			"segmenting requires at least one keyframe", loc(-1))
		return res
	}
	kf := append([]int(nil), keyframes...)
	sort.Ints(kf)
	valid := kf[:0]
	for _, k := range kf {
		if k < 0 || k >= len(tr.PTS) {
			res.Issues.Add(errs.ClassOutOfRange, errs.CodeIndexOffsetOutOfRange,
				fmt.Sprintf("keyframe index %d out of range", k), loc(k))
			continue
		}
		valid = append(valid, k)
	}
	kf = valid
	if len(kf) == 0 {
		res.Issues.AddFatal(errs.ClassDependency, errs.CodeDependencyMissing,
			"no usable keyframes after bounds check", loc(-1))
		return res
	}

	duration := tr.PTS[len(tr.PTS)-1]
	if tr.Duration != nil {
		duration += tr.Duration[len(tr.Duration)-1]
	} else if len(tr.PTS) > 1 {
		duration += tr.PTS[len(tr.PTS)-1] - tr.PTS[len(tr.PTS)-2]
	}
	start := tr.PTS[0]
	// The first cut must land on the first keyframe. A stream that does not
	// begin with a keyframe cannot produce a decodable first segment; the
	// leading samples are reported rather than silently dropped.
	if kf[0] != 0 {
		res.Issues.Add(errs.ClassCorrupt, errs.CodeIndexPendingReview,
			fmt.Sprintf("stream starts with non-keyframe sample %d; leading %d samples unrecoverable",
				kf[0], kf[0]), loc(0))
		res.Pending++
	}
	var boundaries []int
	boundaries = append(boundaries, kf[0])
	next := 1 // first keyframe that may still become a cut
	for {
		target := start + int64(len(boundaries))*cfg.TargetNs
		if target >= duration {
			break
		}
		// The nearest keyframe can sit on either side of the target: clock
		// rounding and drift make exact hits rare. Consider both the first
		// keyframe at or after the target and its predecessor, and never reuse
		// a keyframe already used as a boundary.
		best := -1
		var bestDev int64
		consider := func(k int) {
			if k <= 0 || k <= boundaries[len(boundaries)-1] {
				return
			}
			dev := tr.PTS[k] - target
			if dev < 0 {
				dev = -dev
			}
			if best < 0 || dev < bestDev || (dev == bestDev && k < best) {
				best, bestDev = k, dev
			}
		}
		// Advance next past keyframes strictly before the search window.
		for next < len(kf) && tr.PTS[kf[next]] < target-cfg.MaxNs {
			next++
		}
		atOrAfter := -1
		for i := next; i < len(kf); i++ {
			if tr.PTS[kf[i]] >= target {
				atOrAfter = i
				break
			}
		}
		if atOrAfter >= 0 {
			consider(kf[atOrAfter])
			if atOrAfter > 0 {
				consider(kf[atOrAfter-1])
			}
		} else if len(kf) > 0 {
			// Every remaining keyframe sits before the target; the nearest one
			// on the left is the last keyframe.
			consider(kf[len(kf)-1])
		}
		if best < 0 {
			break
		}
		if bestDev > cfg.MaxBoundaryNs {
			// No keyframe is close enough; the segment ends at the nearest one
			// and the boundary is reported as pending review below.
			boundaries = append(boundaries, best)
			continue
		}
		boundaries = append(boundaries, best)
	}
	// Build cut records with their end boundaries.
	endSample := func(i int) int64 {
		if i+1 < len(boundaries) {
			return tr.PTS[boundaries[i+1]]
		}
		return duration
	}
	var maxDev int64
	for i, b := range boundaries {
		target := start + int64(i)*cfg.TargetNs
		dev := tr.PTS[b] - target
		if dev < 0 {
			dev = -dev
		}
		if dev > maxDev {
			maxDev = dev
		}
		pending := dev > cfg.MaxBoundaryNs
		if pending {
			res.Pending++
			res.Issues.Add(errs.ClassOutOfRange, errs.CodeSegmentBoundary,
				fmt.Sprintf("segment %d boundary deviation %.3f ms exceeds %.3f ms",
					i, float64(dev)/1e6, float64(cfg.MaxBoundaryNs)/1e6), loc(b))
		}
		res.Cuts = append(res.Cuts, Cut{
			Index: i, StartNs: tr.PTS[b], EndNs: endSample(i),
			KeyframeSample: b, TargetNs: target, DeviationNs: dev, Pending: pending,
		})
	}
	res.SegmentCount = len(res.Cuts)
	res.MaxDeviationNs = maxDev
	if res.SegmentCount == 0 {
		res.Issues.AddFatal(errs.ClassDependency, errs.CodeDependencyMissing,
			"no segments produced", loc(-1))
	}
	return res
}

// KeyframeIndexesFromOffsets maps sorted keyframe byte offsets back to sample
// indexes using the index pages that carry both fields.
func KeyframeIndexesFromOffsets(samples []int, offsets []int64, kfOffsets []int64) []int {
	byOffset := make(map[int64]int, len(samples))
	for i, s := range samples {
		if i < len(offsets) {
			byOffset[offsets[i]] = s
		}
	}
	out := make([]int, 0, len(kfOffsets))
	for _, o := range kfOffsets {
		if s, ok := byOffset[o]; ok {
			out = append(out, s)
		}
	}
	sort.Ints(out)
	return out
}
