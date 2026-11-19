package pipeline

import (
	"archivearchivist/internal/model"
)

// ClockView is the per-track clock summary surfaced in JSON.
type ClockView struct {
	Track          string         `json:"track"`
	Wraps          int            `json:"wraps"`
	DriftPPM       model.Rational `json:"drift_ppm"`
	DriftReview    bool           `json:"drift_review"`
	MaxSkewUS      int64          `json:"max_skew_us"`
	Regressions    int            `json:"timestamp_regressions"`
	OriginalHead   int64          `json:"original_head"`
	NormalizedHead int64          `json:"normalized_head"`
	// Points retains every original and normalized timestamp, capped so a
	// pathological capture cannot exhaust memory.
	Points          []ClockPoint `json:"timestamp_points,omitempty"`
	PointsTruncated bool         `json:"timestamp_points_truncated,omitempty"`
}

// ClockPoint is one retained original/normalized timestamp pair.
type ClockPoint struct {
	Seq     uint32 `json:"seq"`
	Orig    int64  `json:"original"`
	Norm    int64  `json:"normalized"`
	Epoch   int64  `json:"epoch"`
	Wrapped bool   `json:"wrapped,omitempty"`
}

// maxClockPoints bounds the retained timestamp history in the JSON report.
const maxClockPoints = 4096
