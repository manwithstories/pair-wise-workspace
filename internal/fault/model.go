// Package fault contains the shared error and damage model used by capture,
// media processing, transactions, and the command adapter.
package fault

import (
	"errors"
	"fmt"
	"math"
)

// Process exit codes required by the command-line interface.
const (
	ExitOK            = 0
	ExitUsage         = 2
	ExitUnrecoverable = 3
	ExitInterrupted   = 75
)

// Input and resident-memory safety limits.
const (
	MaxInputBytes  int64 = 1 << 30 // 1 GiB
	MaxMemoryBytes int64 = 256 << 20
)

// Code classifies a failure for the process boundary.
type Code int

// Error is the common typed error.
type Error struct {
	Code   Code
	Stage  string
	Offset int64
	Err    error
}

func (e *Error) Error() string {
	if e.Offset >= 0 {
		return fmt.Sprintf("%s failed at byte %d: %v", e.Stage, e.Offset, e.Err)
	}
	return fmt.Sprintf("%s failed: %v", e.Stage, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

func ExitCode(err error) int {
	var fe *Error
	if errors.As(err, &fe) {
		return int(fe.Code)
	}
	return ExitUnrecoverable
}

func Usage(stage string, err error) *Error {
	return &Error{Code: ExitUsage, Stage: stage, Offset: -1, Err: err}
}

func Unrecoverable(stage string, err error) *Error {
	return &Error{Code: ExitUnrecoverable, Stage: stage, Offset: -1, Err: err}
}

func AtOffset(stage string, offset int64, err error) *Error {
	return &Error{Code: ExitUnrecoverable, Stage: stage, Offset: offset, Err: err}
}

func Interrupted(stage string, err error) *Error {
	return &Error{Code: ExitInterrupted, Stage: stage, Offset: -1, Err: err}
}

// Damage is a located but usually recoverable input defect.
type Damage struct {
	Offset      int64  `json:"offset"`
	EndOffset   int64  `json:"end_offset"`
	Kind        string `json:"kind"`
	Message     string `json:"message"`
	Recoverable bool   `json:"recoverable"`
}

// Report collects defects and recovery actions without changing their identity.
type Report struct {
	Damages   []Damage `json:"damages"`
	Rollbacks int      `json:"rollbacks"`
	Repairs   []string `json:"repairs"`
}

func (r *Report) Add(d Damage) {
	r.Damages = append(r.Damages, d)
}

func (r *Report) Repair(action string) {
	r.Repairs = append(r.Repairs, action)
}

func (r *Report) Rollback() {
	r.Rollbacks++
}

// CheckInputSize rejects an input larger than the documented limit.
func CheckInputSize(size int64) error {
	if size < 0 {
		return fmt.Errorf("negative input size %d", size)
	}
	if size > MaxInputBytes {
		return fmt.Errorf("input size %d exceeds %d bytes", size, MaxInputBytes)
	}
	return nil
}

// CheckFrameLength validates a declared frame length before allocation.
//
// maxRecord bounds an individual RTP/RTCP record. The subtraction is checked
// explicitly so a malformed length can never drive an integer overflow or a
// huge allocation.
func CheckFrameLength(length uint32, minPayload, fixedTrailer, maxRecord int) (int, error) {
	if length > math.MaxInt32 {
		return 0, fmt.Errorf("frame length %d overflows int", length)
	}
	n := int(length)
	minimum := minPayload + fixedTrailer
	if n < minimum {
		return 0, fmt.Errorf("frame length %d is shorter than minimum %d", n, minimum)
	}
	if n > maxRecord {
		return 0, fmt.Errorf("frame length %d exceeds record limit %d", n, maxRecord)
	}
	payload := n - fixedTrailer
	if payload < minPayload {
		return 0, fmt.Errorf("payload length %d is invalid", payload)
	}
	return payload, nil
}

// SafeAddOffset returns an error instead of wrapping an offset around.
func SafeAddOffset(offset int64, n int) (int64, error) {
	if n < 0 {
		return -1, fmt.Errorf("negative read length %d", n)
	}
	n64 := int64(n)
	if offset < 0 || n64 > math.MaxInt64-offset {
		return -1, fmt.Errorf("offset addition overflow: %d+%d", offset, n)
	}
	return offset + n64, nil
}
