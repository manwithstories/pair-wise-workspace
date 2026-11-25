// Package fault defines the located, exit-code-aware error model used by mediad.
package fault

import (
	"errors"
	"fmt"
)

// Code is a stable machine-readable error identifier.
type Code string

const (
	UnknownVersion     Code = "E_UNKNOWN_VERSION"
	UnsupportedCodec   Code = "E_UNSUPPORTED_CODEC"
	InvalidArgument    Code = "E_INVALID_ARGUMENT"
	IO                 Code = "E_IO"
	InvalidManifest    Code = "E_INVALID_MANIFEST"
	InitMissing        Code = "E_INIT_MISSING"
	Boundary           Code = "E_BOUNDARY"
	BoxTruncated       Code = "E_BOX_TRUNCATED"
	BoxSize            Code = "E_BOX_SIZE"
	MdatCorrupt        Code = "E_MDAT_CORRUPT"
	TimestampOverflow  Code = "E_TIMESTAMP_OVERFLOW"
	TimestampBackward  Code = "E_TIMESTAMP_BACKWARD"
	SegmentMismatch    Code = "E_SEGMENT_MISMATCH"
	CheckpointCorrupt  Code = "E_CHECKPOINT_CORRUPT"
	CheckpointMismatch Code = "E_CHECKPOINT_MISMATCH"
	LedgerCorrupt      Code = "E_LEDGER_CORRUPT"
	ReplayMismatch     Code = "E_REPLAY_MISMATCH"
	Interrupted        Code = "E_INTERRUPTED"
)

// Fault carries a source path and byte offset whenever a location is known.
// Offset is the exact read/parse position. BoxOffset identifies the enclosing
// ISO-BMFF box start when relevant.
type Fault struct {
	Code      Code
	Path      string
	Offset    int64
	BoxOffset int64
	Stage     string
	Err       error
}

func New(code Code, path string, offset int64, format string, args ...any) *Fault {
	return &Fault{Code: code, Path: path, Offset: offset, BoxOffset: -1, Err: fmt.Errorf(format, args...)}
}

func At(code Code, path string, offset, boxOffset int64, err error) *Fault {
	return &Fault{Code: code, Path: path, Offset: offset, BoxOffset: boxOffset, Err: err}
}

func (f *Fault) Error() string {
	msg := string(f.Code)
	if f.Stage != "" {
		msg += " stage=" + f.Stage
	}
	if f.Path != "" {
		msg += " path=" + f.Path
	}
	if f.BoxOffset >= 0 {
		msg += fmt.Sprintf(" box_offset=%d", f.BoxOffset)
	}
	if f.Offset >= 0 {
		msg += fmt.Sprintf(" offset=%d", f.Offset)
	}
	if f.Err != nil {
		msg += ": " + f.Err.Error()
	}
	return msg
}

func (f *Fault) Unwrap() error { return f.Err }

func (f *Fault) WithStage(stage string) *Fault {
	cp := *f
	cp.Stage = stage
	return &cp
}

// As attempts to find a *Fault in err's tree.
func As(err error) (*Fault, bool) {
	var f *Fault
	if errors.As(err, &f) {
		return f, true
	}
	return nil, false
}

// ExitCode maps an error to the CLI contract:
// 1 material, 2 arguments/IO, 75 resumable interruption.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var f *Fault
	if errors.As(err, &f) {
		switch f.Code {
		case Interrupted:
			return 75
		case InvalidArgument:
			return 2
		case IO:
			return 2
		default:
			return 1
		}
	}
	if errors.Is(err, ErrInterrupted) {
		return 75
	}
	return 1
}

// ErrInterrupted is a sentinel for a clean, resumable interruption.
var ErrInterrupted = New(Interrupted, "", -1, "interrupted after durable stage")

// IsInterrupted reports whether err is a resumable interruption.
func IsInterrupted(err error) bool {
	return ExitCode(err) == 75
}
