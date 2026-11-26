package media

import (
	"errors"
	"fmt"
	"strings"
)

// Code is a stable, machine-readable relay error code.
type Code string

const (
	CodeOK               Code = "ok"
	CodeInvalidArgument  Code = "invalid_argument"
	CodeUnsafePath       Code = "unsafe_path"
	CodeInputTooLarge    Code = "input_too_large"
	CodeBoxLength        Code = "invalid_box_length"
	CodeBoxTruncated     Code = "truncated_box"
	CodeIntegerOverflow  Code = "integer_overflow"
	CodeMissingField     Code = "missing_required_box"
	CodeSequenceGap      Code = "mfhd_sequence_gap"
	CodeDuplicatePacket  Code = "duplicate_packet"
	CodeOutOfOrder       Code = "out_of_order_packet"
	CodeLatePacket       Code = "late_packet"
	CodeBurstLoss        Code = "burst_packet_loss"
	CodeRecoveryBudget   Code = "recovery_budget_exhausted"
	CodeTFDTRegression   Code = "tfdt_regression"
	CodeTimelineBreak    Code = "timeline_not_continuous"
	CodeDiscontinuity    Code = "common_discontinuity"
	CodeCacheOrigin      Code = "cache_origin_mismatch"
	CodeCacheDigest      Code = "cache_digest_mismatch"
	CodeCheckpoint       Code = "checkpoint_invalid"
	CodeArtifactMismatch Code = "artifact_mismatch"
	CodeUnrecoverable    Code = "unrecoverable_input"
	CodeInterrupted      Code = "interrupted"
)

// FragmentID identifies one rendition's media fragment.
type FragmentID struct {
	Rendition string `json:"rendition"`
	Segment   uint64 `json:"segment"`
	MFHD      uint32 `json:"mfhd"`
}

// String renders a stable log-safe identifier.
func (id FragmentID) String() string {
	return fmt.Sprintf("rendition=%s segment=%d mfhd=%d", id.Rendition, id.Segment, id.MFHD)
}

// Fault is the common error model shared by parsing, alignment and manifest
// generation. Offset is the first damaged byte and is -1 when byte location is
// not meaningful.
type Fault struct {
	Code        Code       `json:"code"`
	ID          FragmentID `json:"id"`
	Offset      int64      `json:"first_error_byte_offset"`
	Message     string     `json:"message"`
	Recoverable bool       `json:"recoverable"`
	Err         error      `json:"-"`
}

// Error implements error.
func (f *Fault) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s", f.Code)
	if f.ID.Rendition != "" || f.ID.Segment != 0 {
		fmt.Fprintf(&b, " (%s)", f.ID.String())
	}
	if f.Offset >= 0 {
		fmt.Fprintf(&b, " at_byte=%d", f.Offset)
	}
	if f.Message != "" {
		fmt.Fprintf(&b, ": %s", f.Message)
	}
	if f.Err != nil {
		fmt.Fprintf(&b, ": %v", f.Err)
	}
	return b.String()
}

// Unwrap exposes wrapped errors.
func (f *Fault) Unwrap() error { return f.Err }

// NewFault constructs a located fault.
func NewFault(code Code, id FragmentID, offset int64, recoverable bool, format string, args ...any) *Fault {
	return &Fault{Code: code, ID: id, Offset: offset, Recoverable: recoverable, Message: fmt.Sprintf(format, args...)}
}

// AsFault extracts a Fault from an error chain.
func AsFault(err error) (*Fault, bool) {
	var f *Fault
	if errors.As(err, &f) {
		return f, true
	}
	return nil, false
}

// ExitCoder is implemented by errors that map directly to CLI exit codes.
type ExitCoder interface {
	ExitCode() int
}

// ExitError carries the acceptance-specified process exit code.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }
func (e *ExitError) ExitCode() int { return e.Code }

// NewExitError wraps an error with a process exit code.
func NewExitError(code int, err error) *ExitError {
	if err == nil {
		return nil
	}
	return &ExitError{Code: code, Err: err}
}

// ExitCodeOf maps known failures to acceptance exit codes.
func ExitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var ec ExitCoder
	if errors.As(err, &ec) {
		return ec.ExitCode()
	}
	return 1
}
