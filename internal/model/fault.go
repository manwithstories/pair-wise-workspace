package model

import (
	"errors"
	"fmt"
)

// FaultType enumerates every defect class the pipeline can report.
type FaultType string

const (
	FaultTruncated      FaultType = "truncated_packet"
	FaultBadCRC         FaultType = "bad_crc"
	FaultPTSRegression  FaultType = "pts_regression"
	FaultMissingKeyframe FaultType = "missing_keyframe"
	FaultChannelConflict FaultType = "channel_conflict"
	FaultRateConflict   FaultType = "sample_rate_conflict"
	FaultColorConflict  FaultType = "color_tag_conflict"
	FaultUnknownTag     FaultType = "unknown_color_tag"
	FaultPayloadLimit   FaultType = "payload_limit_exceeded"
	FaultBoundaryDrift  FaultType = "boundary_drift"
)

// Fault is the unified error model. Every defect carries the affected track,
// packet sequence number, the nanosecond time range, its type and whether the
// pipeline can continue (recoverable) or must reject the input.
type Fault struct {
	Type        FaultType `json:"type"`
	Track       uint32    `json:"track"`
	PacketSeq   uint64    `json:"packet_seq"`
	StartPTSNS  int64     `json:"start_pts_ns"`
	EndPTSNS    int64     `json:"end_pts_ns"`
	Recoverable bool      `json:"recoverable"`
	Detail      string    `json:"detail"`
}

// Error implements the error interface with a single-line, greppable shape.
func (f *Fault) Error() string {
	return fmt.Sprintf("avpk fault type=%s track=%d seq=%d pts=[%d,%d]ns recoverable=%t: %s",
		f.Type, f.Track, f.PacketSeq, f.StartPTSNS, f.EndPTSNS, f.Recoverable, f.Detail)
}

// AsFault extracts a *Fault from any wrapped error.
func AsFault(err error) (*Fault, bool) {
	var f *Fault
	if errors.As(err, &f) {
		return f, true
	}
	return nil, false
}

// FirstFault scans err chains joined with errors.Join/(%w lists).
func FirstFault(errs []error) *Fault {
	for _, err := range errs {
		if f, ok := AsFault(err); ok {
			return f
		}
	}
	return nil
}

// NewFault is the single constructor so all defects share field semantics.
func NewFault(t FaultType, track uint32, seq uint64, startNS, endNS int64, recoverable bool, detail string) *Fault {
	return &Fault{
		Type:        t,
		Track:       track,
		PacketSeq:   seq,
		StartPTSNS:  startNS,
		EndPTSNS:    endNS,
		Recoverable: recoverable,
		Detail:      detail,
	}
}

// Process exit codes shared by the CLI adapter and the self-check harness.
const (
	ExitOK              = 0
	ExitArgs            = 2
	ExitUnrecoverable   = 3
	ExitSimulatedCrash  = 75
)

// ExitCode maps an error to the documented process exit code. A nil error or
// a chain containing only recoverable faults yields ExitOK.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	f, ok := AsFault(err)
	if !ok {
		return ExitUnrecoverable
	}
	if f.Recoverable {
		return ExitOK
	}
	return ExitUnrecoverable
}
