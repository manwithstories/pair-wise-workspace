// Package mediaerr defines the structured error shared by ingestion, codecs,
// transport, segmentation and resume.
package mediaerr

import (
	"errors"
	"fmt"
)

// Kind is a stable machine-readable error category.
type Kind string

const (
	KindInvalidArgument   Kind = "invalid_argument"
	KindInvalidPacket     Kind = "invalid_packet"
	KindTruncated         Kind = "truncated"
	KindTimestampOverflow Kind = "timestamp_overflow"
	KindBufferLimit       Kind = "buffer_limit"
	KindUnknownVersion    Kind = "unknown_version"
	KindCorruptPayload    Kind = "corrupt_payload"
	KindUnsupportedCodec  Kind = "unsupported_codec"
	KindNoCommonKeyframe  Kind = "no_common_keyframe"
	KindChecksumMismatch  Kind = "checksum_mismatch"
	KindPrefixMismatch    Kind = "prefix_mismatch"
	KindOutputMismatch    Kind = "output_mismatch"
	KindUnrecoverable     Kind = "unrecoverable"
	KindInterrupted       Kind = "interrupted"
)

// Error carries byte/PID context and whether forward progress is possible.
type Error struct {
	Kind        Kind
	Offset      int64
	PID         uint16
	Recoverable bool
	Err         error
}

// New constructs a structured error.
func New(kind Kind, offset int64, pid uint16, recoverable bool, format string, args ...any) *Error {
	return &Error{
		Kind:        kind,
		Offset:      offset,
		PID:         pid,
		Recoverable: recoverable,
		Err:         fmt.Errorf(format, args...),
	}
}

// Wrap attaches context to an existing error while preserving structured data.
func Wrap(kind Kind, offset int64, pid uint16, recoverable bool, err error) *Error {
	if err == nil {
		return nil
	}
	var se *Error
	if errors.As(err, &se) {
		if se.Kind == "" {
			se.Kind = kind
		}
		if se.Offset < 0 && offset >= 0 {
			se.Offset = offset
		}
		if se.PID == 0 && pid != 0 {
			se.PID = pid
		}
		se.Recoverable = se.Recoverable || recoverable
		return se
	}
	return &Error{Kind: kind, Offset: offset, PID: pid, Recoverable: recoverable, Err: err}
}

func (e *Error) Error() string {
	return fmt.Sprintf("mediad %s at offset=%d pid=%d recoverable=%v: %v",
		e.Kind, e.Offset, e.PID, e.Recoverable, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// Is enables errors.Is comparisons by Kind.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Kind == e.Kind
}

// AsError finds the first structured error in err.
func AsError(err error) (*Error, bool) {
	var se *Error
	if errors.As(err, &se) {
		return se, true
	}
	return nil, false
}

// ExitCode maps pipeline errors to the CLI contract. Expected/recoverable
// conditions return zero after handling; unrecoverable input is 2; operational
// and usage failures are 1.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var se *Error
	if errors.As(err, &se) && (se.Kind == KindUnrecoverable || se.Kind == KindNoCommonKeyframe) {
		return 2
	}
	return 1
}
