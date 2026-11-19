// Package fault defines the shared error model used across all pipeline
// stages. Every fault carries the track, media time, processing stage and
// the raw byte offset in the source file so damaged regions can be audited
// and recovery decisions can be reviewed.
package fault

import "fmt"

// Stage identifies the pipeline phase that raised the fault.
type Stage string

const (
	StageScan     Stage = "scan"
	StageTimeline Stage = "timeline"
	StageQueue    Stage = "queue"
	StageRemux    Stage = "remux"
	StageVerify   Stage = "verify"
	StageWrite    Stage = "write"
)

// Kind classifies the fault for programmatic handling.
type Kind string

const (
	KindBoxOutOfRange   Kind = "box_out_of_range"
	KindBoxOversized    Kind = "box_oversized"
	KindTruncated       Kind = "truncated"
	KindMissingIndex    Kind = "missing_index"
	KindTSRegression    Kind = "timestamp_regression"
	KindTSDuplicate     Kind = "timestamp_duplicate"
	KindTSOverflow      Kind = "timestamp_overflow"
	KindAVSyncDrift     Kind = "av_sync_drift"
	KindKeyframeGap     Kind = "keyframe_gap_exceeded"
	KindConsumerSlow    Kind = "consumer_slow"
	KindConsumerError   Kind = "consumer_error"
	KindCanceled        Kind = "canceled"
	KindVerifyMismatch  Kind = "verify_mismatch"
	KindWriteFailure    Kind = "write_failure"
	KindUnrecoverable   Kind = "unrecoverable"
)

// Fault is a single structured error record. Offset is the raw byte offset
// in the source file (-1 when not applicable). Recoverable marks faults the
// remux stage may repair with a policy; unrecoverable faults must never be
// silently skipped.
type Fault struct {
	Kind        Kind   `json:"kind"`
	Stage       Stage  `json:"stage"`
	TrackID     uint32 `json:"track_id,omitempty"`
	MediaTime   int64  `json:"media_time,omitempty"` // unified 90kHz base
	Offset      int64  `json:"offset"`               // raw byte offset in source
	Recoverable bool   `json:"recoverable"`
	Message     string `json:"message"`
}

func (f Fault) Error() string {
	return fmt.Sprintf("[%s/%s] track=%d offset=%d: %s", f.Stage, f.Kind, f.TrackID, f.Offset, f.Message)
}

// New builds a fault with the common fields filled in.
func New(kind Kind, stage Stage, trackID uint32, mediaTime, offset int64, recoverable bool, msg string) Fault {
	return Fault{
		Kind:        kind,
		Stage:       stage,
		TrackID:     trackID,
		MediaTime:   mediaTime,
		Offset:      offset,
		Recoverable: recoverable,
		Message:     msg,
	}
}

// List is an ordered collection of faults with query helpers.
type List []Fault

// Unrecoverable returns the first unrecoverable fault, if any.
func (l List) Unrecoverable() *Fault {
	for i := range l {
		if !l[i].Recoverable {
			return &l[i]
		}
	}
	return nil
}

// ByKind returns all faults of a kind.
func (l List) ByKind(k Kind) List {
	var out List
	for _, f := range l {
		if f.Kind == k {
			out = append(out, f)
		}
	}
	return out
}
