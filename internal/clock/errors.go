// Package clock provides the integer-nanosecond timeline shared by every
// mediarun subsystem, together with the unified error model the pipeline uses
// to distinguish recoverable faults from unrecoverable ones.
package clock

import (
	"errors"
	"fmt"
)

// Kind enumerates the unified error taxonomy. Callers may branch on Kind
// without inspecting message text.
type Kind uint8

const (
	// KindInvalid means the input or manifest was rejected outright.
	KindInvalid Kind = iota + 1
	// KindUnrecoverable means the timeline cannot be uniquely determined.
	KindUnrecoverable
	// KindRecovery means a stage failed and its checkpoint must be rolled back.
	KindRecovery
)

func (k Kind) String() string {
	switch k {
	case KindInvalid:
		return "invalid"
	case KindUnrecoverable:
		return "unrecoverable"
	case KindRecovery:
		return "recovery"
	default:
		return "unknown"
	}
}

// Error is the typed error every mediarun package returns. It carries a Kind,
// a stable machine-readable Code, and an optional subject (segment id, RTP
// sequence, track id) used for provenance.
type Error struct {
	Kind    Kind
	Code    string
	Subject string
	Detail  string
	Err     error
}

func (e *Error) Error() string {
	msg := e.Code
	if e.Subject != "" {
		msg += " [" + e.Subject + "]"
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// New builds a typed error.
func New(kind Kind, code, subject, detail string) *Error {
	return &Error{Kind: kind, Code: code, Subject: subject, Detail: detail}
}

// Wrap builds a typed error that retains the cause.
func Wrap(kind Kind, code, subject string, err error) *Error {
	return &Error{Kind: kind, Code: code, Subject: subject, Err: err}
}

// KindOf extracts the Kind from any error; unknown errors map to KindInvalid
// only when they are explicitly classified by the caller, so the default is
// KindRecovery to force a safe rollback.
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindRecovery
}

// IsKind reports whether err belongs to the given Kind.
func IsKind(err error, k Kind) bool { return err != nil && KindOf(err) == k }

// Stable error codes. Tests and the CLI match these strings.
const (
	CodePathEscape      = "manifest.path_escape"
	CodeDuplicateSeq    = "manifest.duplicate_sequence"
	CodeEntryLimit      = "manifest.entry_limit"
	CodeSizeLimit       = "manifest.size_limit"
	CodeInvalidDuration = "manifest.invalid_duration"
	CodeUnsupported     = "manifest.unsupported"
	CodeWrap            = "clock.timestamp_wrap"
	CodeRegression      = "clock.timestamp_regression"
	CodeDuplicateTS     = "clock.timestamp_duplicate"
	CodeDriftAmbiguous  = "clock.drift_ambiguous"
	CodeEarlyCommit     = "buffer.early_commit"
	CodeSyncThreshold   = "sync.threshold_exceeded"
	CodeSyncInputShort  = "sync.insufficient_input"
	CodeCheckpoint      = "txn.checkpoint_corrupt"
	CodeHashMismatch    = "txn.hash_mismatch"
	CodeCommitInterrupt = "txn.commit_interrupted"
	CodeDiskFull        = "txn.disk_full"
	CodeIndexOffset     = "index.offset_unparsable"
)

// Format renders a short human-readable form for terminal output.
func Format(kind Kind, code string) string { return fmt.Sprintf("%s/%s", kind, code) }

// Code returns the machine-readable code of a mediarun error, or "".
func Code(err error) string {
	if e, ok := err.(*Error); ok {
		return e.Code
	}
	return ""
}
