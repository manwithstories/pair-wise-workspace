package timeline

import "errors"

// Class partitions every pipeline failure into one of four operational
// categories so the CLI can map failures to deterministic exit codes:
//
//   - Gap:       missing media that the pipeline can route around (packet loss).
//   - Corrupt:   malformed media that can be resynchronized or skipped.
//   - Retryable: transient I/O failure; a resume run may succeed.
//   - Fatal:     unrecoverable input; no segment can be produced safely.
//
// Integrity failures (tampered cache or checkpoint) are reported separately
// with ErrIntegrity because they demand a distinct exit code.
type Class int

const (
	ClassUnknown Class = iota
	ClassGap
	ClassCorrupt
	ClassRetryable
	ClassFatal
)

// Error is the structured error shared across packages.
type Error struct {
	Class  Class
	Op     string // package-level operation that failed, e.g. "rtp.reorder"
	Detail string // human readable context
	Err    error  // wrapped cause, may be nil
}

func (e *Error) Error() string {
	msg := e.Op + ": " + e.Detail
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// NewError builds a classified pipeline error.
func NewError(c Class, op, detail string, cause error) *Error {
	return &Error{Class: c, Op: op, Detail: detail, Err: cause}
}

// Sentinel failures.
var (
	// ErrIntegrity means a provenance record, cache entry or checkpoint failed
	// its verification. Such failures are never retried in place.
	ErrIntegrity = errors.New("integrity verification failed")
	// ErrFatalInput means the input stream cannot be restored at all.
	ErrFatalInput = errors.New("unrecoverable input")
	// ErrCheckpoint means checkpoint state is corrupt or was tampered with.
	ErrCheckpoint = errors.New("checkpoint integrity error")
)

// IntegrityError wraps an integrity failure.
func IntegrityError(op, detail string, cause error) *Error {
	return &Error{Class: ClassFatal, Op: op, Detail: detail, Err: errors.Join(ErrIntegrity, cause)}
}
