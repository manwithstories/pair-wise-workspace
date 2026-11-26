// Package model contains cross-subsystem error provenance and recovery state.
package model

import (
	"errors"
	"fmt"
)

// Stage identifies the pipeline boundary that produced or propagated a fault.
type Stage string

const (
	StageGenerate  Stage = "generate"
	StageParseMP4  Stage = "parse_mp4"
	StageParseMXF  Stage = "parse_mxf"
	StageMetadata  Stage = "metadata_resolve"
	StageTransform Stage = "transform"
	StageIndex     Stage = "keyframe_index"
	StageRTP       Stage = "rtp"
	StageRecovery  Stage = "recovery"
	StageSync      Stage = "av_sync"
	StageVerify    Stage = "verify"
)

// Fault is the project-wide error model. Offset is the original byte offset
// in the container or generated packet stream, not a re-based local offset.
type Fault struct {
	Stage   Stage
	TrackID uint32
	Offset  int64
	Code    string
	Err     error
}

func (f *Fault) Error() string {
	base := fmt.Sprintf("%s: code=%s track=%d offset=%d", f.Stage, f.Code, f.TrackID, f.Offset)
	if f.Err != nil {
		return base + ": " + f.Err.Error()
	}
	return base
}

func (f *Fault) Unwrap() error { return f.Err }

// New creates a Fault.
func New(stage Stage, code string, trackID uint32, offset int64, err error) *Fault {
	return &Fault{Stage: stage, Code: code, TrackID: trackID, Offset: offset, Err: err}
}

// At returns a copy with another pipeline stage while preserving origin.
func (f *Fault) At(stage Stage) *Fault {
	cp := *f
	cp.Stage = stage
	return &cp
}

// Annotate wraps err as a Fault unless err already carries the requested
// provenance. Original offset and track are never overwritten by wrapping.
func Annotate(stage Stage, trackID uint32, offset int64, code string, err error) error {
	if err == nil {
		return nil
	}
	var f *Fault
	if errors.As(err, &f) {
		return f.At(stage)
	}
	return New(stage, code, trackID, offset, err)
}

// AsFault finds the first Fault in err's wrap chain.
func AsFault(err error) (*Fault, bool) {
	var f *Fault
	if errors.As(err, &f) {
		return f, true
	}
	return nil, false
}

// FirstOffset returns the original raw offset from a Fault chain, or -1 when
// the error has no byte provenance.
func FirstOffset(err error) int64 {
	f, ok := AsFault(err)
	if !ok {
		return -1
	}
	return f.Offset
}

// RecoveryState records how one segment or frame reached the verifier.
type RecoveryState string

const (
	StateOriginal      RecoveryState = "original"
	StateDuplicate     RecoveryState = "duplicate_dropped"
	StateReordered     RecoveryState = "reordered"
	StateRetransmitted RecoveryState = "retransmitted"
	StateXORRecovered  RecoveryState = "xor_recovered"
	StateConcealed     RecoveryState = "silence_concealed"
	StateDropped       RecoveryState = "damaged_dropped"
	StateKeyframeWait  RecoveryState = "waiting_keyframe"
	StateRejected      RecoveryState = "metadata_rejected"
	StateIsolated      RecoveryState = "packet_isolated"
)

// SegmentRef preserves source provenance beside recovery state.
type SegmentRef struct {
	Index       int
	TrackID     uint32
	SourceOff   int64
	State       RecoveryState
	OriginError *Fault
}
