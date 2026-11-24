package core

import (
	"errors"
	"fmt"
)

// MaxFragmentBytes is the enforced single-fragment size ceiling (128 MiB).
const MaxFragmentBytes int64 = 128 << 20

// Kind classifies a Fault so subsystems can branch without matching strings.
type Kind string

const (
	KindBoxLength   Kind = "box_length"    // declared box size is impossible
	KindOutOfBounds Kind = "out_of_bounds" // read/reference past the buffer
	KindTruncated   Kind = "truncated"     // box or buffer ended early
	KindBadField    Kind = "bad_field"     // structurally readable but invalid field
	KindOverflow    Kind = "overflow"      // integer/rational overflow
	KindUnsupported Kind = "unsupported"   // feature this build cannot decode
	KindDigest      Kind = "digest_mismatch"
)

// Fault is the canonical parse/validation error. Offset is the byte offset of
// the first malformed structure within the fragment; -1 means "not offset
// addressable" (for example a configuration fault).
type Fault struct {
	Kind   Kind
	Path   string
	Offset int64
	Err    error
}

// Faultf builds a Fault without a wrapped sentinel error.
func Faultf(kind Kind, offset int64, format string, args ...any) *Fault {
	return &Fault{Kind: kind, Offset: offset, Err: fmt.Errorf(format, args...)}
}

// WrapFault attaches path/offset/kind context to an underlying error.
func WrapFault(kind Kind, path string, offset int64, err error) *Fault {
	return &Fault{Kind: kind, Path: path, Offset: offset, Err: err}
}

func (f *Fault) Error() string {
	where := ""
	if f.Path != "" {
		where = f.Path + ": "
	}
	if f.Offset >= 0 {
		return fmt.Sprintf("%s%s at offset %d: %v", where, f.Kind, f.Offset, f.Err)
	}
	return fmt.Sprintf("%s%s: %v", where, f.Kind, f.Err)
}

// Unwrap exposes the wrapped error.
func (f *Fault) Unwrap() error { return f.Err }

// AsFault finds the first *Fault in err's chain.
func AsFault(err error) (*Fault, bool) {
	var f *Fault
	if errors.As(err, &f) {
		return f, true
	}
	return nil, false
}

// Exit codes mandated by the terminal contract.
const (
	ExitComplete    = 0 // full delivery
	ExitQuarantined = 2 // corrupt pieces isolated, manifest still valid
	ExitCanceled    = 3 // canceled, resumable checkpoint written
	ExitFatal       = 1 // unrecoverable input or configuration error
)

// ExitCoder is implemented by errors that map directly to a process exit code.
type ExitCoder interface{ ExitCode() int }

// ConfigError marks an unrecoverable invocation/configuration problem.
type ConfigError struct{ Err error }

func (e *ConfigError) Error() string { return "config: " + e.Err.Error() }
func (e *ConfigError) Unwrap() error { return e.Err }
func (*ConfigError) ExitCode() int   { return ExitFatal }

// InputError marks an unrecoverable input problem (missing root, digest bind
// mismatch, fragment over the size ceiling, ...).
type InputError struct{ Err error }

func (e *InputError) Error() string { return "input: " + e.Err.Error() }
func (e *InputError) Unwrap() error { return e.Err }
func (*InputError) ExitCode() int   { return ExitFatal }

// CanceledError reports cooperative cancellation that left a valid checkpoint.
type CanceledError struct{ Err error }

func (e *CanceledError) Error() string {
	if e.Err != nil {
		return "canceled: " + e.Err.Error()
	}
	return "canceled"
}
func (e *CanceledError) Unwrap() error { return e.Err }
func (*CanceledError) ExitCode() int   { return ExitCanceled }

// ExitCode maps any error to its process exit code. Nil maps to 0. Errors that
// do not implement ExitCoder are treated as unrecoverable (1).
func ExitCode(err error) int {
	if err == nil {
		return ExitComplete
	}
	var ec ExitCoder
	if errors.As(err, &ec) {
		return ec.ExitCode()
	}
	return ExitFatal
}
