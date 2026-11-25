// Package timeline provides the unified media clock used by slicing,
// silence detection, and transaction validation.
package timeline

import (
	"errors"
	"fmt"
	"time"
)

// PCRModulus is the range of the 33-bit MPEG-TS PCR_base field.
const PCRModulus uint64 = 1 << 33

// PCRHz is the time base of PCR_base and PTS/DTS.
const PCRHz = 90_000

// ErrorKind classifies a unified timeline failure.
type ErrorKind string

const (
	ErrPCRRange         ErrorKind = "pcr_range"
	ErrPCRBackward      ErrorKind = "pcr_backward"
	ErrClockUndecidable ErrorKind = "clock_undecidable"
	ErrSegmentInvalid   ErrorKind = "segment_crosses_invalid_interval"
	ErrUnsupportedCodec ErrorKind = "unsupported_codec"
	ErrTransaction      ErrorKind = "transaction_invariant"
)

// Error is the unified error type for cross-module timeline decisions.
type Error struct {
	Kind ErrorKind
	Msg  string
	Err  error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Kind, e.Msg, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Kind, e.Msg)
}

func (e *Error) Unwrap() error { return e.Err }

// Sentinel errors allow errors.Is while retaining structured details.
var (
	ErrUndecidable = errors.New("media clock is not decidable")
	ErrInvalidSpan = errors.New("segment spans an invalid timeline interval")
)

func NewError(kind ErrorKind, msg string, err error) *Error {
	return &Error{Kind: kind, Msg: msg, Err: err}
}

func IsUndecidable(err error) bool {
	var te *Error
	if errors.As(err, &te) {
		return te.Kind == ErrClockUndecidable || te.Kind == ErrPCRRange || te.Kind == ErrPCRBackward
	}
	return errors.Is(err, ErrUndecidable)
}

// Interval is a half-open [Start, End) range on the unified clock.
type Interval struct {
	Start     time.Duration `json:"start"`
	End       time.Duration `json:"end"`
	Reason    string        `json:"reason"`
	Recovered bool          `json:"recovered"`
}

// Contains reports whether t lies in the half-open interval.
func (in Interval) Contains(t time.Duration) bool {
	return t >= in.Start && t < in.End
}

// Intersects reports whether two half-open intervals overlap.
func (in Interval) Intersects(start, end time.Duration) bool {
	return start < in.End && end > in.Start
}

// Clock extends 33-bit PCR values and records gap/invalid ranges.
//
// Clock is not safe for concurrent use; a pipeline should feed it from one
// parser goroutine and pass immutable snapshots to downstream workers.
type Clock struct {
	lastRaw   *uint64
	unwrapped uint64
	wrapCount int
	Last      time.Duration
	Gaps      []Interval
	Invalid   []Interval
}

// WrapCount returns the number of PCR wrap events observed.
func (c *Clock) WrapCount() int { return c.wrapCount }

// ExtendPCR converts a raw 33-bit PCR_base value to a monotonically extended
// counter. A modular delta near +modulus is interpreted as a wrap.
func (c *Clock) ExtendPCR(raw uint64) (uint64, error) {
	if raw >= PCRModulus {
		return 0, NewError(ErrPCRRange, fmt.Sprintf("PCR %d exceeds 33 bits", raw), nil)
	}
	if c.lastRaw == nil {
		v := raw
		c.lastRaw = &v
		c.unwrapped = raw
		c.Last = PCRDuration(raw)
		return c.unwrapped, nil
	}

	prev := *c.lastRaw
	// Signed modular distance. Values more than half the range away are
	// assumed to be a wrap rather than a genuine huge jump.
	delta := int64(raw) - int64(prev)
	if delta > int64(PCRModulus/2) {
		delta -= int64(PCRModulus)
	} else if delta < -int64(PCRModulus/2) {
		delta += int64(PCRModulus)
		c.wrapCount++
	}
	if delta < 0 {
		// Small backward movements can occur in malformed/reordered input but
		// make exact duration decisions unsafe. Callers mark the range invalid.
		return c.unwrapped, NewError(ErrPCRBackward,
			fmt.Sprintf("PCR moved backward: raw %d after %d", raw, prev), ErrUndecidable)
	}

	c.unwrapped += uint64(delta)
	*c.lastRaw = raw
	c.Last = PCRDuration(c.unwrapped)
	return c.unwrapped, nil
}

// FeedPCR extends raw PCR and returns its unified duration.
func (c *Clock) FeedPCR(raw uint64) (time.Duration, error) {
	full, err := c.ExtendPCR(raw)
	if err != nil {
		return 0, err
	}
	return PCRDuration(full), nil
}

// PCRDuration converts 90 kHz ticks to time.Duration.
func PCRDuration(ticks uint64) time.Duration {
	return time.Duration(ticks) * time.Second / PCRHz
}

// MarkGap records a known missing-but-bounded interval. Media inside it must
// not be synthesized.
func (c *Clock) MarkGap(start, end time.Duration, reason string) Interval {
	if end < start {
		end = start
	}
	in := Interval{Start: start, End: end, Reason: reason, Recovered: true}
	c.Gaps = append(c.Gaps, in)
	return in
}

// MarkInvalid records a range where the clock cannot decide exact timing.
func (c *Clock) MarkInvalid(start, end time.Duration, reason string) Interval {
	if end < start {
		end = start
	}
	in := Interval{Start: start, End: end, Reason: reason}
	c.Invalid = append(c.Invalid, in)
	return in
}

// IntersectsInvalid reports whether [start,end) touches an invalid range.
func (c *Clock) IntersectsInvalid(start, end time.Duration) bool {
	for _, in := range c.Invalid {
		if in.Intersects(start, end) {
			return true
		}
	}
	return false
}

// ValidateSpan rejects a proposed clip that crosses a clock-invalid range.
func (c *Clock) ValidateSpan(start, end time.Duration) error {
	for _, in := range c.Invalid {
		if in.Intersects(start, end) {
			return NewError(ErrSegmentInvalid,
				fmt.Sprintf("clip [%s,%s) crosses invalid interval %q [%s,%s)",
					start, end, in.Reason, in.Start, in.End), ErrInvalidSpan)
		}
	}
	return nil
}
