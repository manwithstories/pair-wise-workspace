// Package clock defines the single nanosecond timeline used by every track.
//
// All cut intervals are half-open: [Start, End).  The package also owns the
// shared structured error model so ingest, alignment and cutting can preserve
// the originating track and raw timestamp.
package clock

import (
	"errors"
	"fmt"
	"math/big"
)

// Ns is an absolute or relative time in nanoseconds.
type Ns int64

const (
	NsPerSecond Ns = 1_000_000_000
	NsPerMs     Ns = 1_000_000
)

// ErrorCode is a stable, machine-readable failure class.
type ErrorCode string

const (
	CodeInvalidArgument ErrorCode = "invalid_argument"
	CodeOverflow        ErrorCode = "integer_overflow"
	CodeNegative        ErrorCode = "negative_duration"
	CodeMissingPacket   ErrorCode = "missing_packet"
	CodeCycle           ErrorCode = "dependency_cycle"
	CodeColorConflict   ErrorCode = "color_conflict"
	CodeTruncatedMP4    ErrorCode = "truncated_mp4"
	CodeBadInterval     ErrorCode = "bad_interval"
	CodeBitrate         ErrorCode = "bitrate_mismatch"
	CodeCoverage        ErrorCode = "coverage_gap"
	CodeConfig          ErrorCode = "config_error"
	CodeWrite           ErrorCode = "write_failed"
)

// Error is the common error model shared across the pipeline.
type Error struct {
	Code              ErrorCode `json:"error_code"`
	TrackID           int       `json:"track_id"`
	Message           string    `json:"message"`
	OriginalTimestamp int64     `json:"original_timestamp,omitempty"`
	OriginalValue     string    `json:"original_value,omitempty"`
	StartNs           Ns        `json:"start_ns,omitempty"`
	EndNs             Ns        `json:"end_ns,omitempty"`
	Wrapping          error     `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.TrackID >= 0 {
		return fmt.Sprintf("%s track_id=%d: %s", e.Code, e.TrackID, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Wrapping }

// AsError converts an arbitrary error to *Error when possible.
func AsError(err error) (*Error, bool) {
	var ce *Error
	if errors.As(err, &ce) {
		return ce, true
	}
	return nil, false
}

func Errorf(code ErrorCode, trackID int, format string, args ...any) *Error {
	return &Error{Code: code, TrackID: trackID, Message: fmt.Sprintf(format, args...)}
}

// Interval is a half-open nanosecond range [Start, End).
type Interval struct {
	Start Ns `json:"start_ns"`
	End   Ns `json:"end_ns"`
}

func (i Interval) Duration() Ns { return i.End - i.Start }

// Valid reports whether the interval can participate in the shared timeline.
func (i Interval) Valid() error {
	if i.Start < 0 || i.End < 0 {
		return &Error{Code: CodeNegative, TrackID: -1, StartNs: i.Start, EndNs: i.End,
			Message: "interval contains a negative timestamp"}
	}
	if i.End < i.Start {
		return &Error{Code: CodeBadInterval, TrackID: -1, StartNs: i.Start, EndNs: i.End,
			Message: "half-open interval ends before it starts"}
	}
	return nil
}

func (i Interval) Contains(t Ns) bool { return i.Start <= t && t < i.End }

func (i Interval) Intersects(other Interval) bool {
	return i.Start < other.End && other.Start < i.End
}

// TicksToNs converts a rational time base.  One tick represents
// timeBaseNumerator/timeBaseDenominator seconds.
//
// All arithmetic is range-checked.  Negative input is converted, but callers
// are expected to reject negative intervals through Interval.Valid.
func TicksToNs(ticks int64, timeBaseNumerator, timeBaseDenominator int64) (Ns, error) {
	if timeBaseNumerator <= 0 || timeBaseDenominator <= 0 {
		return 0, Errorf(CodeInvalidArgument, -1,
			"time base must be positive: %d/%d", timeBaseNumerator, timeBaseDenominator)
	}
	n := new(big.Int).Mul(big.NewInt(ticks), big.NewInt(timeBaseNumerator))
	n.Mul(n, big.NewInt(int64(NsPerSecond)))
	q := new(big.Int).Quo(n, big.NewInt(timeBaseDenominator))
	if !q.IsInt64() {
		return 0, &Error{Code: CodeOverflow, TrackID: -1, OriginalTimestamp: ticks,
			OriginalValue: fmt.Sprintf("ticks=%d timebase=%d/%d", ticks, timeBaseNumerator, timeBaseDenominator),
			Message:       "rational timestamp overflows int64 nanoseconds"}
	}
	return Ns(q.Int64()), nil
}

// SamplesToNs converts an integral sample index at sampleRate Hz.
func SamplesToNs(sampleIndex int64, sampleRate int) (Ns, error) {
	if sampleRate <= 0 {
		return 0, Errorf(CodeInvalidArgument, -1, "sample rate must be positive: %d", sampleRate)
	}
	return TicksToNs(sampleIndex, 1, int64(sampleRate))
}

// RoundToSample rounds t to the nearest legal PCM sample and returns both the
// sample index and its exact nanosecond position.
func RoundToSample(t Ns, sampleRate int) (int64, Ns, error) {
	if sampleRate <= 0 {
		return 0, 0, Errorf(CodeInvalidArgument, -1, "sample rate must be positive: %d", sampleRate)
	}
	if t < 0 {
		return 0, 0, &Error{Code: CodeNegative, TrackID: -1, OriginalTimestamp: int64(t),
			Message: "cannot round negative time to a sample"}
	}
	num := new(big.Int).Mul(big.NewInt(int64(t)), big.NewInt(int64(sampleRate)))
	num.Add(num, big.NewInt(int64(NsPerSecond)/2))
	num.Quo(num, big.NewInt(int64(NsPerSecond)))
	if !num.IsInt64() {
		return 0, 0, &Error{Code: CodeOverflow, TrackID: -1, OriginalTimestamp: int64(t),
			Message: "sample index overflows int64"}
	}
	idx := num.Int64()
	rounded, err := SamplesToNs(idx, sampleRate)
	if err != nil {
		return 0, 0, err
	}
	return idx, rounded, nil
}

// Max returns the later timestamp.
func Max(a, b Ns) Ns {
	if a > b {
		return a
	}
	return b
}

// Min returns the earlier timestamp.
func Min(a, b Ns) Ns {
	if a < b {
		return a
	}
	return b
}
