package model

import "errors"

// FaultKind classifies every defect with a stable machine-readable string.
type FaultKind string

const (
	FaultTruncatedBox     FaultKind = "truncated_box"
	FaultTruncatedTable   FaultKind = "truncated_sample_table"
	FaultInvalidLength    FaultKind = "invalid_sample_length"
	FaultOffsetOutOfRange FaultKind = "sample_offset_out_of_range"
	FaultClipOutOfBounds  FaultKind = "clip_point_out_of_bounds"
	FaultMissingSync      FaultKind = "missing_sync_samples"
	FaultRTPSeqGap        FaultKind = "rtp_sequence_gap"
	FaultRTPBoundary      FaultKind = "rtp_packet_boundary"
	FaultUnparseableTrack FaultKind = "unparseable_track"
	FaultBlockDamage      FaultKind = "damaged_block"
)

// Sentinel errors map to the CLI exit-code contract:
//
//	exit 2 -> damaged input / bad arguments
//	exit 3 -> checkpoint mismatch / index write failure
var (
	ErrDamagedInput       = errors.New("input data is damaged")
	ErrInvalidArgs        = errors.New("invalid arguments")
	ErrCheckpointMismatch = errors.New("checkpoint does not match current input")
	ErrIndexWrite         = errors.New("index write failed")
	ErrLimitExceeded      = errors.New("input exceeds configured limits")
)

// Fault carries both a byte position and a time position. The Has* flags
// distinguish a real zero coordinate from an absent one.
type Fault struct {
	Kind        FaultKind `json:"kind"`
	Source      string    `json:"source"`
	TrackID     uint32    `json:"track_id"`
	ByteOffset  int64     `json:"byte_offset"`
	HasByte     bool      `json:"has_byte"`
	SampleIndex int64     `json:"sample_index"` // 0-based; -1 when unknown
	TimeNS      int64     `json:"time_ns"`
	HasTime     bool      `json:"has_time"`
	Message     string    `json:"message"`
}

// NewByteFault builds a fault located at a byte offset.
func NewByteFault(kind FaultKind, source string, off int64, msg string) Fault {
	return Fault{
		Kind:        kind,
		Source:      source,
		SampleIndex: -1,
		ByteOffset:  off,
		HasByte:     true,
		Message:     msg,
	}
}

// NewSampleFault builds a fault located at a sample boundary, carrying both
// the byte offset and the unified time of the sample.
func NewSampleFault(kind FaultKind, source string, trackID uint32, idx uint32, ev *SampleEvent, msg string) Fault {
	f := Fault{
		Kind:        kind,
		Source:      source,
		TrackID:     trackID,
		SampleIndex: int64(idx),
		Message:     msg,
	}
	if ev != nil {
		f.ByteOffset = int64(ev.Offset)
		f.HasByte = true
		f.TimeNS = ev.StartNS
		f.HasTime = true
	}
	return f
}

// DamageSegment is a maximal run of consecutive faults on one track. Bounds
// always snap to sample boundaries (or box boundaries when no sample table is
// available), so the locating error never exceeds one sample.
type DamageSegment struct {
	Source      string   `json:"source"`
	TrackID     uint32   `json:"track_id"`
	StartOffset int64    `json:"start_offset"`
	EndOffset   int64    `json:"end_offset"`
	StartNS     int64    `json:"start_ns"`
	EndNS       int64    `json:"end_ns"`
	Kinds       []string `json:"kinds"`
	FaultCount  int      `json:"fault_count"`
}

// ExitCode maps a run-level error onto the documented process exit codes.
func ExitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, ErrCheckpointMismatch), errors.Is(err, ErrIndexWrite):
		return 3
	case errors.Is(err, ErrDamagedInput), errors.Is(err, ErrInvalidArgs),
		errors.Is(err, ErrLimitExceeded):
		return 2
	default:
		return 2
	}
}
