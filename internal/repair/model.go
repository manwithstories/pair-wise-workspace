// Package repair recovers samples from a probe timeline. Each track is
// recovered independently: scanning stops at the first damaged sample and
// restarts at the next verifiable sync sample. Recovered timestamps keep the
// original order; damaged samples never enter the recovered set.
package repair

import (
	"math"

	"mediacheck/internal/fault"
	"mediacheck/internal/probe"
)

// Config controls recovery limits.
type Config struct {
	// SegmentMS is the shared bucket width.
	SegmentMS int64
	// ToleranceMS is the allowed cross-layer boundary disagreement.
	ToleranceMS int64
	// CacheBytes bounds the reusable read buffer.
	CacheBytes int64
	// MaxIssues bounds retained faults per track.
	MaxIssues int
	// DetectUniformFill enables the uniform-byte corruption heuristic.
	DetectUniformFill bool
	// FillBytes lists the byte values treated as erased payload. The default
	// covers 0x00 and 0xff, the two values left by interrupted and padded
	// writes.
	FillBytes []byte
}

// DefaultConfig returns the recovery defaults.
func DefaultConfig() Config {
	return Config{
		SegmentMS:         2000,
		ToleranceMS:       1,
		CacheBytes:        1 << 20,
		MaxIssues:         32,
		DetectUniformFill: true,
		FillBytes:         []byte{0x00, 0xff},
	}
}

// Normalize replaces non-positive fields with defaults.
func (c Config) Normalize() Config {
	d := DefaultConfig()
	if c.SegmentMS <= 0 {
		c.SegmentMS = d.SegmentMS
	}
	if c.ToleranceMS <= 0 {
		c.ToleranceMS = d.ToleranceMS
	}
	if c.CacheBytes <= 0 {
		c.CacheBytes = d.CacheBytes
	}
	if c.MaxIssues <= 0 {
		c.MaxIssues = d.MaxIssues
	}
	if len(c.FillBytes) == 0 {
		c.FillBytes = d.FillBytes
	}
	if c.CacheBytes > 8<<20 {
		c.CacheBytes = 8 << 20
	}
	return c
}

// TrackStatus is the recovery outcome for one track.
type TrackStatus string

const (
	StatusOK            TrackStatus = "ok"
	StatusRecovered     TrackStatus = "recovered"
	StatusUnrecoverable TrackStatus = "unrecoverable"
)

// Status is the recovery outcome for the whole input.
type Status string

const (
	ResultComplete Status = "complete"
	ResultPartial  Status = "partial"
	ResultFailed   Status = "failed"
)

// Damage is one located corruption or truncation event.
type Damage struct {
	TrackID       uint32     `json:"track_id"`
	Code          fault.Code `json:"code"`
	Offset        int64      `json:"offset"`
	SampleIndex   uint64     `json:"sample_index"`
	SampleCount   int        `json:"sample_count"`
	ExcludedBytes int64      `json:"excluded_bytes"`
	StartMS       int64      `json:"start_ms"`
	// ResumeMS is when recovery restarted (-1 if it never did).
	ResumeMS int64  `json:"resume_ms"`
	Detail   string `json:"detail,omitempty"`
}

// Span is a recovered or unrecoverable time range.
type Span struct {
	StartMS     int64  `json:"start_ms"`
	EndMS       int64  `json:"end_ms"`
	Samples     int    `json:"samples"`
	Bytes       int64  `json:"bytes"`
	Keyframes   int    `json:"keyframes"`
	FirstSample uint64 `json:"first_sample"`
	LastSample  uint64 `json:"last_sample"`
	// Complete marks a span with an unbroken timeline.
	Complete bool `json:"complete"`
}

// Segment is one recovered shared-clock bucket for a track.
type Segment struct {
	Index      int   `json:"index"`
	StartMS    int64 `json:"start_ms"`
	Samples    int   `json:"samples"`
	Bytes      int64 `json:"bytes"`
	Keyframes  int   `json:"keyframes"`
	FirstDTSMS int64 `json:"first_dts_ms"`
	LastDTSMS  int64 `json:"last_dts_ms"`
	FirstCTSMS int64 `json:"first_cts_ms"`
	LastCTSMS  int64 `json:"last_cts_ms"`
	DurationMS int64 `json:"duration_ms"`
	Recovered  bool  `json:"recovered"`
}

// TrackResult is the independent recovery result for one track.
type TrackResult struct {
	TrackID    uint32      `json:"track_id"`
	Kind       probe.Kind  `json:"kind"`
	Codec      string      `json:"codec"`
	Timescale  uint32      `json:"timescale"`
	StartMS    int64       `json:"start_ms"`
	DurationMS int64       `json:"duration_ms"`
	Status     TrackStatus `json:"status"`

	Recovered       Span      `json:"recovered"`
	Segments        []Segment `json:"segments"`
	Unrecoverable   []Span    `json:"unrecoverable"`
	Damages         []Damage  `json:"damages"`
	DiscardedBytes  int64     `json:"discarded_bytes"`
	FirstUnreadable int64     `json:"first_unreadable_offset"`
	// ExcludedSamples counts samples left out of the manifest.
	ExcludedSamples uint64 `json:"excluded_samples"`
	// KeyframeGaps are consecutive recovered keyframe deltas in ms.
	KeyframeGaps []int64 `json:"-"`

	Issues []*fault.Fault `json:"-"`
}

// Result is the combined recovery and boundary outcome.
type Result struct {
	Path     string         `json:"path"`
	Status   Status         `json:"status"`
	Tracks   []*TrackResult `json:"tracks"`
	Boundary BoundaryReport `json:"boundary"`
	Issues   []*fault.Fault `json:"-"`
}

// addIssue appends a fault up to the configured cap.
func (t *TrackResult) addIssue(f *fault.Fault, max int) {
	if len(t.Issues) < max {
		t.Issues = append(t.Issues, f)
	}
}

// usable reports whether any sample was recovered.
func (t *TrackResult) usable() bool { return t.Recovered.Samples > 0 }

// clampMS bounds an int64 to the signed range after saturation.
func clampMS(v int64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	if v < math.MinInt64 {
		return math.MinInt64
	}
	return v
}
