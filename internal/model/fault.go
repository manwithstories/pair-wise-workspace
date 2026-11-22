// Package model defines the shared domain types exchanged across the
// ingest, inspection, repair and remux subsystems.
package model

import "fmt"

// Kind classifies a detected defect. The four generator-reproducible
// families required by the specification are represented explicitly.
type Kind string

const (
	KindTruncatedBox    Kind = "truncated_box"
	KindCorruptSample   Kind = "corrupt_sample"
	KindMissingKeyframe Kind = "missing_keyframe"
	KindClockDrift      Kind = "clock_drift"
	KindInvalidTimebase Kind = "invalid_timebase"
	KindKeyframeGap     Kind = "keyframe_gap"
	KindRTPGap          Kind = "rtp_gap"
	KindOrphanEvent     Kind = "orphan_event"
	KindBufferOverflow  Kind = "buffer_overflow"
	KindWriteFailure    Kind = "write_failure"
)

// Severity orders defects for stable reporting.
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Recovery states for a fault and, by extension, for a segment.
type Recovery string

const (
	RecPending Recovery = "pending"
	// RecRecovered means media continuity was restored (e.g. reorder)
	// and no timeline content needed to be dropped.
	RecRecovered Recovery = "recovered"
	// RecQuarantined means the damaged span was isolated; later valid
	// spans remain processable. The original gap is never hidden.
	RecQuarantined Recovery = "quarantined"
	// RecUnrecovered means the defect could neither be repaired nor
	// safely isolated before the end of the input.
	RecUnrecovered Recovery = "unrecovered"
)

// Fault is a defect anchored to a track and a half-open time range
// [StartMs, EndMs) on the unified timeline.
type Fault struct {
	ID       int      `json:"id"`
	Kind     Kind     `json:"kind"`
	Severity Severity `json:"severity"`
	Track    string   `json:"track"`
	StartMs  int64    `json:"start_ms"`
	EndMs    int64    `json:"end_ms"`
	Detail   string   `json:"detail"`
	Recovery Recovery `json:"recovery"`
}

// String renders a stable, grep-friendly single-line representation.
func (f Fault) String() string {
	return fmt.Sprintf("fault#%d %s track=%s [%d,%d)ms %s %s",
		f.ID, f.Kind, f.Track, f.StartMs, f.EndMs, f.Recovery, f.Detail)
}

// Segment is a timeline span covering one or more adjacent faults.
// A quarantined segment is excluded from remuxed media but recorded
// verbatim in the output manifest.
type Segment struct {
	ID       int      `json:"id"`
	Track    string   `json:"track"`
	StartMs  int64    `json:"start_ms"`
	EndMs    int64    `json:"end_ms"`
	FaultIDs []int    `json:"fault_ids"`
	Recovery Recovery `json:"recovery"`
	Reason   Kind     `json:"reason"`
}

// CommittedBoundary is the last timeline point durably handed to the
// sink. On cancel or write failure everything past it is uncommitted.
type CommittedBoundary struct {
	Track   string `json:"track"`
	EndMs   int64  `json:"end_ms"`
	Segment int    `json:"segment_id"`
}

// ExitCode mirrors the documented CLI contract.
type ExitCode int

const (
	ExitOK          ExitCode = 0
	ExitDamaged     ExitCode = 1
	ExitInvalidArgs ExitCode = 2
	ExitInternal    ExitCode = 3
)
