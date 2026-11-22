// Package clock defines the single time base used by ingest, timeline and
// delivery. Event times are always microseconds since the first RTP timestamp
// observed on a stream. Codec clock rates only exist at the ingest boundary.
package clock

import (
	"errors"
	"fmt"
)

// MicrosPerSecond is the internal time unit (1 us).
const MicrosPerSecond int64 = 1_000_000

// Jump limits reject clearly illegal clock transitions while still allowing
// retransmissions and normal out-of-order delivery.
const (
	MaxForwardJumpUS  int64 = 10 * 60 * MicrosPerSecond
	MaxBackwardJumpUS int64 = 10 * 60 * MicrosPerSecond
)

var (
	// ErrZeroRate reports an unusable codec clock rate.
	ErrZeroRate = errors.New("clock: rate must be greater than zero")
	// ErrNonPositiveDuration reports a duration that cannot be placed on the
	// timeline. Durations must be strictly positive.
	ErrNonPositiveDuration = errors.New("clock: duration must be positive")
)

// Rate is a codec clock rate in ticks per second (for example 48000 or 90000).
type Rate uint32

// Validate reports whether the rate can be used for conversions.
func (r Rate) Validate() error {
	if r == 0 {
		return ErrZeroRate
	}
	return nil
}

// TicksToUS converts codec ticks to the unified microsecond base. The result is
// truncated toward zero, which is deterministic and keeps 90 kHz AAC ticks
// representable without floating point drift.
func (r Rate) TicksToUS(ticks int64) int64 {
	return ticks * MicrosPerSecond / int64(r)
}

// USToTicks converts microseconds to codec ticks.
func (r Rate) USToTicks(us int64) int64 {
	return us * int64(r) / MicrosPerSecond
}

// DeltaUS converts a tick difference to microseconds.
func (r Rate) DeltaUS(from, to int64) int64 {
	return r.TicksToUS(to - from)
}

// JumpError describes a rejected clock transition.
type JumpError struct {
	DeltaUS int64
}

func (e *JumpError) Error() string {
	return fmt.Sprintf("clock: illegal timestamp jump of %d us", e.DeltaUS)
}

// ValidateJump rejects transitions beyond the configured forward/backward
// limits. Sequence wrap is removed before this check by the unwrappers below.
func ValidateJump(deltaUS int64) error {
	if deltaUS > MaxForwardJumpUS || deltaUS < -MaxBackwardJumpUS {
		return &JumpError{DeltaUS: deltaUS}
	}
	return nil
}

// ValidateDuration rejects zero, negative and absurdly long packet durations.
func ValidateDuration(us int64) error {
	if us <= 0 {
		return ErrNonPositiveDuration
	}
	if us > 5*MicrosPerSecond {
		return fmt.Errorf("clock: duration %d us exceeds 5s packet limit", us)
	}
	return nil
}

// TimestampUnwrapper removes 32-bit RTP timestamp wrap. Reordered packets are
// supported because deltas are interpreted as signed 32-bit values.
type TimestampUnwrapper struct {
	initialized bool
	last        uint32
	extended    int64
}

// Unwrap returns the extended timestamp for v.
func (u *TimestampUnwrapper) Unwrap(v uint32) int64 {
	if !u.initialized {
		u.initialized = true
		u.last = v
		u.extended = int64(v)
		return u.extended
	}
	u.extended += int64(int32(v - u.last))
	u.last = v
	return u.extended
}

// State exports the unwrapper state for checkpointing.
func (u *TimestampUnwrapper) State() (last uint32, extended int64, ok bool) {
	return u.last, u.extended, u.initialized
}

// Restore reloads a checkpointed unwrapper state.
func (u *TimestampUnwrapper) Restore(last uint32, extended int64, ok bool) {
	u.last, u.extended, u.initialized = last, extended, ok
}

// SequenceUnwrapper removes 16-bit RTP sequence wrap.
type SequenceUnwrapper struct {
	initialized bool
	last        uint16
	extended    int64
}

// Unwrap returns the extended sequence number for v.
func (u *SequenceUnwrapper) Unwrap(v uint16) int64 {
	if !u.initialized {
		u.initialized = true
		u.last = v
		u.extended = int64(v)
		return u.extended
	}
	u.extended += int64(int16(v - u.last))
	u.last = v
	return u.extended
}

// State exports the unwrapper state for checkpointing.
func (u *SequenceUnwrapper) State() (last uint16, extended int64, ok bool) {
	return u.last, u.extended, u.initialized
}

// Restore reloads a checkpointed unwrapper state.
func (u *SequenceUnwrapper) Restore(last uint16, extended int64, ok bool) {
	u.last, u.extended, u.initialized = last, extended, ok
}
