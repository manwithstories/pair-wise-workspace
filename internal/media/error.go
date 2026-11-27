package media

import (
	"errors"
	"fmt"
)

// FaultKind classifies a Fault so the buffer, recovery and replay stages
// can share one error model.
type FaultKind int

const (
	FaultNone FaultKind = iota
	FaultParse
	FaultLoss
	FaultDuplicate
	FaultReorder
	FaultLate
	FaultBufferOverflow
	FaultClockJump
	FaultLayoutMissing
	FaultColorConflict
	FaultCheckpointCorrupt
	FaultUnrecoverable
)

var kindNames = map[FaultKind]string{
	FaultNone:              "none",
	FaultParse:             "parse",
	FaultLoss:              "loss",
	FaultDuplicate:         "duplicate",
	FaultReorder:           "reorder",
	FaultLate:              "late",
	FaultBufferOverflow:    "buffer-overflow",
	FaultClockJump:         "clock-jump",
	FaultLayoutMissing:     "layout-missing",
	FaultColorConflict:     "color-conflict",
	FaultCheckpointCorrupt: "checkpoint-corrupt",
	FaultUnrecoverable:     "unrecoverable",
}

func (k FaultKind) String() string {
	if s, ok := kindNames[k]; ok {
		return s
	}
	return fmt.Sprintf("fault-%d", int(k))
}

// Fault is the unified error carried across all pipeline stages. The At
// field records the input arrival index so operators can locate which
// input packet an anomaly started from.
type Fault struct {
	Kind  FaultKind
	Op    string // subsystem operation, e.g. "ingest.rtp"
	Track uint32 // SSRC, 0 when not applicable
	Seq   uint64 // extended sequence number, when applicable
	At    int64  // input arrival index, -1 when not applicable
	Msg   string
}

func (f *Fault) Error() string {
	return fmt.Sprintf("%s: %s track=%d seq=%d at=%d: %s",
		f.Op, f.Kind, f.Track, f.Seq, f.At, f.Msg)
}

// IsFault reports whether err is a Fault of the given kind.
func IsFault(err error, kind FaultKind) bool {
	var f *Fault
	if errors.As(err, &f) {
		return f.Kind == kind
	}
	return false
}
