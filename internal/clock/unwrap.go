// Package clock maintains the unified media time base. It converts raw 32-bit
// RTP timestamps (which wrap and drift) into a monotonic 64-bit tick space
// shared by the audio and video subsystems.
package clock

import (
	"errors"
	"fmt"
)

// MaxRTP is the 32-bit RTP timestamp modulus (RFC 3550 §5.1).
const MaxRTP uint64 = 1 << 32

// ErrUncertain marks a time interval whose mapping cannot be trusted because
// calibration data is insufficient. It is a marker, not a guess: callers must
// keep the interval visible instead of silently repairing it.
var ErrUncertain = errors.New("clock: uncertain interval, calibration data insufficient")

// Unwrapper extends 32-bit RTP timestamps to a monotonic 64-bit series.
// WrapPoint lets a synthetic or legacy stream wrap below the full 2^32
// modulus; real RTP uses WrapPoint == MaxRTP.
type Unwrapper struct {
	SSRC      uint32
	WrapPoint uint64
	clockRate uint32
	lastSeq   uint16
	haveLast  bool
	lastExt   uint64 // last unwrapped timestamp, in RTP clock units
	lastBase  uint64 // extended base of the cycle containing lastExt
	wrapCount uint64
}

// NewUnwrapper builds an unwrapper. wrapPoint 0 means the full RTP modulus.
func NewUnwrapper(ssrc uint32, clockRate uint32, wrapPoint uint64) *Unwrapper {
	if wrapPoint == 0 {
		wrapPoint = MaxRTP
	}
	return &Unwrapper{SSRC: ssrc, WrapPoint: wrapPoint, clockRate: clockRate}
}

// WrapCount reports how many wrap events have been observed so far.
func (u *Unwrapper) WrapCount() uint64 { return u.wrapCount }

// Unwrap maps one (sequence number, raw timestamp) pair to an extended
// timestamp. A backwards jump larger than half the wrap modulus is treated as
// a wrap; after a wrap, a raw value that fits the previous cycle is a late
// packet and replayed there without mutating learned state.
func (u *Unwrapper) Unwrap(seq uint16, raw uint32) (uint64, error) {
	r := uint64(raw)
	if !u.haveLast {
		u.haveLast = true
		u.lastSeq, u.lastBase, u.lastExt = seq, 0, r
		return r, nil
	}
	// Detect a forward wrap: raw fell behind the current offset by more than
	// half the modulus, so the packet belongs to the next cycle.
	offset := u.lastExt - u.lastBase
	if offset > r && offset-r > u.WrapPoint/2 {
		u.wrapCount++
	}
	// Resolve against the current and previous cycles; nearest extended time
	// wins, which keeps retransmits in their original older cycle.
	cur := u.wrapCount*u.WrapPoint + r
	best, bestCycle := cur, u.wrapCount
	if u.wrapCount > 0 {
		prev := cur - u.WrapPoint
		if absDiff(prev, u.lastExt) < absDiff(cur, u.lastExt) {
			best, bestCycle = prev, u.wrapCount-1
		}
	}
	if absDiff(best, u.lastExt) > u.WrapPoint/2 {
		return 0, fmt.Errorf("clock: timestamp jump at seq %d exceeds half modulus", seq)
	}
	// A replay into an older cycle leaves the learned history untouched.
	if bestCycle < u.wrapCount {
		return best, nil
	}
	u.lastSeq = seq
	u.lastBase = u.wrapCount * u.WrapPoint
	u.lastExt = best
	return best, nil
}

func absDiff(a, b uint64) uint64 {
	if a > b {
		return a - b
	}
	return b - a
}

// Reset clears learned state so a new SSRC incarnation starts fresh.
func (u *Unwrapper) Reset() {
	*u = Unwrapper{SSRC: u.SSRC, WrapPoint: u.WrapPoint, clockRate: u.clockRate}
}
