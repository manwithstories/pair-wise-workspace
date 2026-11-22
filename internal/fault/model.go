// Package fault defines the error classification shared by probe, repair and
// fanout. Audio and video use the same codes and the same time base so a
// damaged sample is reported identically no matter which track it belongs to.
package fault

import (
	"errors"
	"fmt"
)

// Code classifies a fault independently of the track kind.
type Code string

const (
	CodeIO              Code = "io"
	CodeStructure       Code = "structure"
	CodeTruncated       Code = "truncated"
	CodeCorrupt         Code = "corrupt"
	CodeIndexMissing    Code = "index_missing"
	CodeIndexIncomplete Code = "index_incomplete"
	CodeTimestamp       Code = "timestamp"
	CodeBoundary        Code = "boundary"
	CodeUnsupported     Code = "unsupported"
	CodeInvalidArgument Code = "invalid_argument"
	CodeUnrecoverable   Code = "unrecoverable"
)

// Severity describes how a fault affects the analysis.
type Severity string

const (
	// SeverityFatal means the input cannot be analyzed at all.
	SeverityFatal Severity = "fatal"
	// SeverityPartial means the affected range is unusable but other data is.
	SeverityPartial Severity = "partial"
	// SeverityWarning records an anomaly that does not remove data.
	SeverityWarning Severity = "warning"
)

// Fault is a locatable, classified error.
type Fault struct {
	Code     Code     `json:"code"`
	Severity Severity `json:"severity"`
	Op       string   `json:"op,omitempty"`
	TrackID  uint32   `json:"track_id,omitempty"`
	Offset   int64    `json:"offset"`
	Detail   string   `json:"detail,omitempty"`
	Cause    error    `json:"-"`
}

// New builds a fault with an unset offset (-1).
func New(code Code, sev Severity, op, detail string) *Fault {
	return &Fault{Code: code, Severity: sev, Op: op, Offset: -1, Detail: detail}
}

// At builds a fault located at a file offset.
func At(code Code, sev Severity, op string, offset int64, detail string) *Fault {
	f := New(code, sev, op, detail)
	f.Offset = offset
	return f
}

// ForTrack builds a track-scoped fault.
func ForTrack(code Code, sev Severity, op string, trackID uint32, offset int64, detail string) *Fault {
	f := At(code, sev, op, offset, detail)
	f.TrackID = trackID
	return f
}

// Wrap attaches a cause to a classified fault.
func Wrap(code Code, sev Severity, op string, cause error) *Fault {
	f := New(code, sev, op, "")
	f.Cause = cause
	return f
}

// Error implements error.
func (f *Fault) Error() string {
	if f == nil {
		return "<nil>"
	}
	msg := string(f.Code)
	if f.Op != "" {
		msg = f.Op + ": " + msg
	}
	if f.TrackID != 0 {
		msg = fmt.Sprintf("%s (track %d)", msg, f.TrackID)
	}
	if f.Offset >= 0 {
		msg = fmt.Sprintf("%s at %d", msg, f.Offset)
	}
	if f.Detail != "" {
		msg += ": " + f.Detail
	}
	if f.Cause != nil {
		msg += ": " + f.Cause.Error()
	}
	return msg
}

// Unwrap exposes the wrapped cause.
func (f *Fault) Unwrap() error {
	if f == nil {
		return nil
	}
	return f.Cause
}

// As extracts the first Fault in an error chain.
func As(err error) (*Fault, bool) {
	var f *Fault
	if errors.As(err, &f) {
		return f, true
	}
	return nil, false
}

// CodeOf returns the code of the first classified error, or "" when none.
func CodeOf(err error) Code {
	if f, ok := As(err); ok {
		return f.Code
	}
	return ""
}

// IsCode reports whether err carries the given classification.
func IsCode(err error, code Code) bool {
	return CodeOf(err) == code
}

// ExitRank orders faults for deterministic reporting (most severe first).
func ExitRank(code Code) int {
	switch code {
	case CodeInvalidArgument:
		return 0
	case CodeIO, CodeStructure, CodeUnsupported, CodeUnrecoverable:
		return 1
	case CodeTruncated, CodeCorrupt, CodeIndexMissing, CodeIndexIncomplete, CodeBoundary:
		return 2
	default:
		return 3
	}
}
