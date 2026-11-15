// Package model defines the single shared in-memory manifest model used by
// both the manifest generator and the packager. No other package is
// imported here; every other layer depends on this one only.
package model

import (
	"fmt"
	"math/big"
)

// Problem is a structured finding with a stable error code so that reports
// and exit codes can be derived without string matching.
type Problem struct {
	Code string
	Rep  string
	Msg  string
}

func (p Problem) Error() string {
	if p.Rep != "" {
		return fmt.Sprintf("%s rep=%s %s", p.Code, p.Rep, p.Msg)
	}
	return fmt.Sprintf("%s %s", p.Code, p.Msg)
}

const (
	CodeDupID            = "E_DUP_ID"
	CodeBadBandwidth     = "E_BAD_BANDWIDTH"
	CodeZeroTimescale    = "E_ZERO_TIMESCALE"
	CodeZeroDuration     = "E_ZERO_DURATION"
	CodeTimeRegression   = "E_TIME_REGRESSION"
	CodeMissingSegment   = "E_MISSING_SEGMENT"
	CodePeriodOrder      = "E_PERIOD_ORDER"
	CodeNegativeTime     = "E_NEGATIVE_TIME"
	CodeOutOfRange       = "E_OUT_OF_RANGE"
	CodeUnknownHolder    = "E_UNKNOWN_PLACEHOLDER"
	CodeTemplateConflict = "E_TEMPLATE_CONFLICT"
	CodeReadbackMismatch = "E_READBACK_MISMATCH"
	CodeBadFrameRate     = "E_BAD_FRAMERATE"
	CodeBadGeometry      = "E_BAD_GEOMETRY"
	CodeBadCodec         = "E_BAD_CODEC"
	CodeNumberGap        = "E_NUMBER_GAP"
)

// MPD is the root of the self-developed manifest.
type MPD struct {
	ID                        string
	Type                      string   // "static"
	MediaPresentationDuration *big.Rat // seconds
	Periods                   []*Period
}

// Period holds one period; Start is the absolute presentation start in
// seconds and may be discontinuous with the previous period.
type Period struct {
	ID    string
	Start *big.Rat
	Sets  []*AdaptationSet
}

// AdaptationSet groups representations of one content type.
type AdaptationSet struct {
	ID          int
	ContentType string // "video" | "audio"
	Reps        []*Representation
}

// Representation describes one ladder rung.
type Representation struct {
	ID        string
	Bandwidth int64
	Width     int
	Height    int
	FrameRate string // rational, e.g. "30000/1001"; empty for audio
	Codecs    string
	Template  *SegmentTemplate
}

// SegmentTemplate carries either a SegmentTimeline or a SegmentList,
// never both. PresentationTimeOffset is in timescale ticks.
type SegmentTemplate struct {
	Timescale              uint64
	PresentationTimeOffset *big.Rat
	StartNumber            uint64
	Media                  string
	Initialization         string
	Timeline               *SegmentTimeline
	List                   *SegmentList
}

// SegmentList is only supported for the mutual-exclusion check.
type SegmentList struct{ URLs []string }

// SegmentTimeline is a run-length coded list of S elements.
type SegmentTimeline struct{ S []*S }

// S is one timeline run: T optional start tick, D duration ticks, R repeat.
type S struct {
	T *uint64
	D uint64
	R int64
}

// Reps flattens all representations across periods in document order.
func (m *MPD) Reps() []*Representation {
	var out []*Representation
	for _, p := range m.Periods {
		for _, as := range p.Sets {
			out = append(out, as.Reps...)
		}
	}
	return out
}

// Validate enforces global invariants: unique representation IDs,
// positive bandwidth, non-zero timescale, template/list mutual exclusion
// and monotonically non-decreasing period starts.
func (m *MPD) Validate() []Problem {
	var probs []Problem
	for _, period := range m.Periods {
		seen := map[string]bool{} // IDs must be unique within a period
		for _, as := range period.Sets {
			for _, rep := range as.Reps {
				if seen[rep.ID] {
					probs = append(probs, Problem{Code: CodeDupID, Rep: rep.ID, Msg: "duplicate representation id"})
				}
				seen[rep.ID] = true
				if rep.Bandwidth <= 0 {
					probs = append(probs, Problem{Code: CodeBadBandwidth, Rep: rep.ID, Msg: "bandwidth must be > 0"})
				}
				if rep.Template == nil {
					continue
				}
				if rep.Template.Timescale == 0 {
					probs = append(probs, Problem{Code: CodeZeroTimescale, Rep: rep.ID, Msg: "timescale must be > 0"})
				}
				if rep.Template.Timeline != nil && rep.Template.List != nil {
					probs = append(probs, Problem{Code: CodeTemplateConflict, Rep: rep.ID, Msg: "SegmentTemplate and SegmentList are mutually exclusive"})
				}
			}
		}
	}
	var prev *big.Rat
	for i, p := range m.Periods {
		if p.Start == nil {
			continue
		}
		if p.Start.Sign() < 0 {
			probs = append(probs, Problem{Code: CodeNegativeTime, Msg: fmt.Sprintf("period %d has negative start", i)})
		}
		if prev != nil && p.Start.Cmp(prev) < 0 {
			probs = append(probs, Problem{Code: CodePeriodOrder, Msg: fmt.Sprintf("period %d starts before previous period", i)})
		}
		prev = p.Start
	}
	return probs
}
