// Package manifest aligns per-bitrate representations onto common segment
// boundaries on the shared time base. Cut points always fall back to a
// decodable keyframe; the fallback offset is recorded per boundary.
package manifest

import (
	"fmt"

	"archivecheck/internal/clock"
	"archivecheck/internal/fault"
)

// Representation is one bitrate layer with its usable keyframe times.
type Representation struct {
	Name         string       `json:"name"`
	BitrateKbps  int          `json:"bitrate_kbps"`
	KeyframeNums []int64      `json:"keyframe_samples"` // 1-based sync sample numbers
	Keyframes    []clock.Time `json:"keyframes"`        // presentation times, ascending
	Duration     clock.Time   `json:"duration"`
}

// RepBoundary records where one representation actually cuts for a target
// boundary: the keyframe it fell back to and the offset that introduced.
type RepBoundary struct {
	Rep            string     `json:"rep"`
	Target         clock.Time `json:"target"`
	Actual         clock.Time `json:"actual"`
	KeyframeSample int64      `json:"keyframe_sample"`
	SkewMs         int64      `json:"skew_ms"`
}

// Segment is one aligned interval across all representations.
type Segment struct {
	Index      int           `json:"index"`
	StartMs    int64         `json:"start_ms"`
	EndMs      int64         `json:"end_ms"`
	Boundaries []RepBoundary `json:"boundaries"`
}

// Manifest is the aligned segmentation result.
type Manifest struct {
	SegmentDurationMs int64      `json:"segment_duration_ms"`
	Segments          []Segment  `json:"segments"`
	Representations   int        `json:"representations"`
	MaxBoundarySkewMs int64      `json:"max_boundary_skew_ms"`
	Faults            fault.List `json:"faults,omitempty"`
}

// Align snaps every representation onto common boundaries spaced segDur
// apart. A representation with no keyframe at or before a boundary yields a
// missing-keyframe fault naming the common boundary and the worst skew.
func Align(reps []Representation, segDur clock.Time) (*Manifest, error) {
	if len(reps) == 0 {
		return nil, fmt.Errorf("manifest: no representations")
	}
	m := &Manifest{SegmentDurationMs: segDur.Millis(), Representations: len(reps)}

	dur := reps[0].Duration
	for _, r := range reps[1:] {
		if r.Duration.Cmp(dur) > 0 {
			dur = r.Duration
		}
	}
	// Number of intervals covering [0, dur).
	nSeg := 0
	for t := clock.Zero; t.Cmp(dur) < 0; nSeg++ {
		nt, err := t.Add(segDur)
		if err != nil {
			return nil, err
		}
		t = nt
	}

	for k := 0; k <= nSeg; k++ {
		target := clock.Zero
		for i := 0; i < k; i++ {
			nt, err := target.Add(segDur)
			if err != nil {
				return nil, err
			}
			target = nt
		}
		seg := Segment{Index: k, StartMs: target.Millis()}
		end, err := target.Add(segDur)
		if err != nil {
			return nil, err
		}
		seg.EndMs = end.Millis()
		var worst int64
		var missing []string
		for _, r := range reps {
			rb := RepBoundary{Rep: r.Name, Target: target}
			// Largest keyframe at or before the target.
			best := -1
			for i, kt := range r.Keyframes {
				if kt.Cmp(target) <= 0 {
					best = i
				} else {
					break
				}
			}
			if best < 0 {
				missing = append(missing, r.Name)
				continue
			}
			rb.Actual = r.Keyframes[best]
			rb.KeyframeSample = r.KeyframeNums[best]
			skew, err := target.Sub(rb.Actual)
			if err != nil {
				return nil, err
			}
			rb.SkewMs = skew.Millis()
			if rb.SkewMs > worst {
				worst = rb.SkewMs
			}
			seg.Boundaries = append(seg.Boundaries, rb)
		}
		if len(missing) > 0 {
			m.Faults = append(m.Faults, &fault.Fault{
				Kind: fault.KindMissingKeyframe, Track: 0, Sample: -1, Offset: -1,
				Chain: []string{"segment", "checksum"},
				Message: fmt.Sprintf("representations %v lack a keyframe at or before common boundary %d ms; max skew at boundary = %d ms",
					missing, target.Millis(), worst),
			})
		}
		if worst > m.MaxBoundarySkewMs {
			m.MaxBoundarySkewMs = worst
		}
		if k < nSeg {
			m.Segments = append(m.Segments, seg)
		}
	}
	return m, nil
}
