// Package clock continuation: interval coverage sets, the shared error model,
// and deterministic percentile helpers used by every reporting stage.
package clock

import (
	"math"
	"sort"
)

// Interval is a half-open unified-timeline range [StartUs, EndUs).
type Interval struct {
	StartUs int64 `json:"start_us"`
	EndUs   int64 `json:"end_us"`
}

// NewInterval validates and constructs an interval.
func NewInterval(startUs, endUs int64) (Interval, bool) {
	if endUs <= startUs {
		return Interval{}, false
	}
	return Interval{StartUs: startUs, EndUs: endUs}, true
}

// DurationMS returns the interval length in milliseconds.
func (iv Interval) DurationMS() float64 {
	if iv.EndUs <= iv.StartUs {
		return 0
	}
	return float64(iv.EndUs-iv.StartUs) / float64(UsPerMS)
}

// StartMS returns the unified start in milliseconds.
func (iv Interval) StartMS() float64 { return float64(iv.StartUs) / float64(UsPerMS) }

// EndMS returns the unified end in milliseconds.
func (iv Interval) EndMS() float64 { return float64(iv.EndUs) / float64(UsPerMS) }

// Overlaps reports whether two half-open intervals share media time.
func (iv Interval) Overlaps(other Interval) bool {
	return iv.StartUs < other.EndUs && other.StartUs < iv.EndUs
}

// Coverage is an unordered set of disjoint media-time intervals.
type Coverage []Interval

// Merge sorts and merges overlapping or touching intervals.
func Merge(intervals []Interval) Coverage {
	if len(intervals) == 0 {
		return nil
	}
	cp := make([]Interval, len(intervals))
	copy(cp, intervals)
	sort.SliceStable(cp, func(i, j int) bool {
		if cp[i].StartUs != cp[j].StartUs {
			return cp[i].StartUs < cp[j].StartUs
		}
		return cp[i].EndUs < cp[j].EndUs
	})
	out := make(Coverage, 0, len(cp))
	for _, iv := range cp {
		if iv.EndUs <= iv.StartUs {
			continue
		}
		if len(out) == 0 || iv.StartUs > out[len(out)-1].EndUs {
			out = append(out, iv)
			continue
		}
		if iv.EndUs > out[len(out)-1].EndUs {
			out[len(out)-1].EndUs = iv.EndUs
		}
	}
	return out
}

// TotalMS returns the covered wall time in milliseconds.
func (c Coverage) TotalMS() float64 {
	var total int64
	for _, iv := range c {
		total += iv.EndUs - iv.StartUs
	}
	return float64(total) / float64(UsPerMS)
}

// Contains reports whether us falls inside the coverage.
func (c Coverage) Contains(us int64) bool {
	for _, iv := range c {
		if us >= iv.StartUs && us < iv.EndUs {
			return true
		}
	}
	return false
}

// Span returns the outer [min start, max end) of the coverage.
func (c Coverage) Span() (Interval, bool) {
	if len(c) == 0 {
		return Interval{}, false
	}
	return Interval{StartUs: c[0].StartUs, EndUs: c[len(c)-1].EndUs}, true
}

// Gaps returns uncovered ranges within window whose length is at least
// minGapUs. The result is deterministic and sorted.
func (c Coverage) Gaps(window Interval, minGapUs int64) Coverage {
	if window.EndUs <= window.StartUs {
		return nil
	}
	normalized := Merge(c)
	var gaps Coverage
	cursor := window.StartUs
	for _, iv := range normalized {
		if iv.EndUs <= cursor {
			continue
		}
		if iv.StartUs >= window.EndUs {
			break
		}
		start := iv.StartUs
		if start < cursor {
			start = cursor
		}
		if start > cursor && start-cursor >= minGapUs {
			gaps = append(gaps, Interval{StartUs: cursor, EndUs: start})
		}
		if iv.EndUs > cursor {
			cursor = iv.EndUs
		}
		if cursor >= window.EndUs {
			break
		}
	}
	if cursor < window.EndUs && window.EndUs-cursor >= minGapUs {
		gaps = append(gaps, Interval{StartUs: cursor, EndUs: window.EndUs})
	}
	return gaps
}

// Intersect returns the intersection of two coverages.
func Intersect(a, b Coverage) Coverage {
	var out Coverage
	for _, x := range Merge(a) {
		for _, y := range Merge(b) {
			start, end := x.StartUs, x.EndUs
			if y.StartUs > start {
				start = y.StartUs
			}
			if y.EndUs < end {
				end = y.EndUs
			}
			if end > start {
				out = append(out, Interval{StartUs: start, EndUs: end})
			}
		}
	}
	return Merge(out)
}

// ErrorKind is a stable label used in structured output.
type ErrorKind string

const (
	ErrBoxSizeOverrun    ErrorKind = "box_size_overrun"
	ErrBoxTruncated      ErrorKind = "box_truncated"
	ErrSegmentTruncated  ErrorKind = "segment_truncated"
	ErrResync            ErrorKind = "resync"
	ErrSampleOutOfBounds ErrorKind = "sample_out_of_bounds"
	ErrMfhdGap           ErrorKind = "mfhd_gap"
	ErrMfhdDuplicate     ErrorKind = "mfhd_duplicate"
	ErrTfdtRegression    ErrorKind = "tfdt_regression"
	ErrKeyframeConflict  ErrorKind = "keyframe_conflict"
	ErrRecoveredPartial  ErrorKind = "recovered_partial"
	ErrMissingRecovered  ErrorKind = "missing_unrecoverable"
	ErrRTPLost           ErrorKind = "rtp_lost"
	ErrRTPOutOfWindow    ErrorKind = "rtp_out_of_window"
	ErrBoundaryMismatch  ErrorKind = "boundary_mismatch"
)

// LocatedError is an error annotated with both the unified media position and
// the original container offset. PositionKnown is false when the error could
// not be tied to a trustworthy media time.
type LocatedError struct {
	Kind          ErrorKind `json:"kind"`
	AtUs          int64     `json:"at_us"`
	PositionKnown bool      `json:"position_known"`
	TrackID       uint32    `json:"track_id,omitempty"`
	Offset        uint64    `json:"offset"`
	Segment       int       `json:"segment"`
	Detail        string    `json:"detail,omitempty"`
}

// ErrorModel is the shared error accounting for a representation. It is not
// safe for concurrent mutation; each representation owns one instance.
type ErrorModel struct {
	SyncToleranceMS     float64        `json:"sync_tolerance_ms"`
	BoundaryToleranceMS float64        `json:"boundary_tolerance_ms"`
	Errors              []LocatedError `json:"errors"`
}

// NewErrorModel returns an error model with the given tolerances.
func NewErrorModel(syncToleranceMS, boundaryToleranceMS float64) *ErrorModel {
	return &ErrorModel{
		SyncToleranceMS:     syncToleranceMS,
		BoundaryToleranceMS: boundaryToleranceMS,
	}
}

// Record appends an error.
func (m *ErrorModel) Record(e LocatedError) {
	if m == nil {
		return
	}
	m.Errors = append(m.Errors, e)
}

// Counts returns the number of errors per kind.
func (m *ErrorModel) Counts() map[string]int {
	out := make(map[string]int)
	if m == nil {
		return out
	}
	for _, e := range m.Errors {
		out[string(e.Kind)]++
	}
	return out
}

// Percentile returns the nearest-rank percentile of values (0 <= p <= 1).
// The input is copied and sorted, so callers may pass unsorted samples.
func Percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	if p <= 0 {
		p = 0
	}
	if p >= 1 {
		p = 1
	}
	cp := make([]float64, len(values))
	copy(cp, values)
	sort.Float64s(cp)
	idx := int(math.Ceil(p*float64(len(cp)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}
