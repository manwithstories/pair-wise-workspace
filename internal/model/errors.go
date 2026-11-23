package model

import "fmt"

// ErrorCode classifies every locatable failure produced by the pipeline.
type ErrorCode string

const (
	ErrHeader         ErrorCode = "header_invalid"
	ErrTruncated      ErrorCode = "record_truncated"
	ErrChecksum       ErrorCode = "record_checksum_mismatch"
	ErrKind           ErrorCode = "record_kind_unknown"
	ErrIndexDigest    ErrorCode = "index_digest_mismatch"
	ErrIndexIncomplete ErrorCode = "index_incomplete"
	ErrIndexField     ErrorCode = "index_field_missing"
	ErrTimeline       ErrorCode = "timeline_unrecoverable"
	ErrCancelled      ErrorCode = "pipeline_cancelled"
	ErrWrite          ErrorCode = "sink_write_failed"
)

// LocatableError carries a machine-readable code together with the byte
// offset and (when applicable) the track/record sequence where it occurred.
type LocatableError struct {
	Code    ErrorCode
	Offset  int64 // -1 when unknown / not applicable
	Kind    uint16
	Seq     uint32
	Message string
}

// NewError builds a locatable error with an unknown offset.
func NewError(code ErrorCode, format string, args ...any) *LocatableError {
	return &LocatableError{Code: code, Offset: -1, Message: fmt.Sprintf(format, args...)}
}

// At attaches a byte offset and returns the same error.
func (e *LocatableError) At(offset int64) *LocatableError { e.Offset = offset; return e }

// WithRecord attaches track kind and record sequence.
func (e *LocatableError) WithRecord(kind uint16, seq uint32) *LocatableError {
	e.Kind, e.Seq = kind, seq
	return e
}

func (e *LocatableError) Error() string {
	loc := "offset=?"
	if e.Offset >= 0 {
		loc = fmt.Sprintf("offset=%d", e.Offset)
	}
	if e.Seq != 0 || e.Kind != 0 {
		return fmt.Sprintf("%s: %s kind=%d seq=%d: %s", e.Code, loc, e.Kind, e.Seq, e.Message)
	}
	return fmt.Sprintf("%s: %s: %s", e.Code, loc, e.Message)
}

// AsLocatable extracts a *LocatableError from err when present.
func AsLocatable(err error) (*LocatableError, bool) {
	if err == nil {
		return nil, false
	}
	if le, ok := err.(*LocatableError); ok {
		return le, true
	}
	return nil, false
}
