package timebase

import (
	"fmt"

	"github.com/pion/rtp"

	"mediarun/internal/mediaerr"
)

// Unwrapper expands modular counters (RTP 16-bit sequence numbers, 32-bit
// timestamps, MXF wrap-around counters) into a monotonic int64 stream.
//
// A step is treated as a normal wraparound only when continuity is
// decidable: the signed modular delta must lie within half of the counter
// range, per the RFC 3550 update rule. Any larger jump is ambiguous (a
// dropped burst vs. a reset vs. corruption), so instead of guessing the
// code reports a structured SEQUENCE_GAP error carrying the precise delta
// and the unwrapped position of the previous sample.
type Unwrapper struct {
	modulus int64 // 2^bits
	half    int64
	mask    uint64
	maxJump int64 // largest tolerated forward step (beyond it: SEQUENCE_GAP)
	started bool
	last    int64 // last emitted unwrapped value
	count   int64 // number of observed values (position of next sample)
}

// NewUnwrapper builds an unwrapper for a counter of 16 or 32 bits. The
// forward jump ceiling follows RTP practice: 100 for 16-bit sequence
// numbers, 1,000,000 for 32-bit timestamps (~11 s on a 90 kHz clock).
func NewUnwrapper(bits uint) *Unwrapper {
	if bits != 16 && bits != 32 {
		panic("timebase: unwrapper supports 16 or 32 bit counters only")
	}
	mod := int64(1) << bits
	maxJump := int64(100)
	if bits == 32 {
		maxJump = 1_000_000
	}
	return &Unwrapper{modulus: mod, half: mod / 2, mask: uint64(mod - 1), maxJump: maxJump}
}

// WithMaxJump overrides the tolerated forward step and returns the receiver.
func (u *Unwrapper) WithMaxJump(jump int64) *Unwrapper {
	u.maxJump = jump
	return u
}

// Observe feeds one raw counter value and returns its unwrapped, monotonic
// equivalent. The first value establishes the origin (mapped to itself).
// A non-positive modular delta is always decidable (wraparound/back-step);
// a forward delta above the jump ceiling is an ambiguous discontinuity and
// reported rather than guessed.
func (u *Unwrapper) Observe(raw uint64) (int64, error) {
	raw &= u.mask
	if !u.started {
		u.started = true
		u.last = int64(raw)
		u.count = 1
		return u.last, nil
	}
	// Signed modular delta: positive => forward, negative => wraparound/back.
	delta := int64(raw) - (u.last & (u.modulus - 1))
	if delta >= u.half {
		delta -= u.modulus
	} else if delta < -u.half {
		delta += u.modulus
	}
	if delta > u.maxJump {
		pos := u.count
		return 0, mediaerr.New(mediaerr.CodeSequenceGap, fmt.Sprintf(
			"counter discontinuity at sample %d: raw=%#x prev_raw=%#x forward_delta=%d exceeds_max=%d",
			pos, raw, u.last&(u.modulus-1), delta, u.maxJump)).
			At("", "", pos)
	}
	next := u.last + delta
	if next < 0 {
		return 0, mediaerr.New(mediaerr.CodeSequenceGap, fmt.Sprintf(
			"unwrapped value went negative at sample %d: %d", u.count, next))
	}
	u.last = next
	u.count++
	return next, nil
}

// Last returns the most recent unwrapped value and whether any value has
// been observed yet.
func (u *Unwrapper) Last() (int64, bool) { return u.last, u.started }

// Observed returns how many raw values have been fed in.
func (u *Unwrapper) Observed() int64 { return u.count }

// TrackUnwrapper unwraps both the RTP sequence number and the RTP timestamp
// of one track, keeping the two counters aligned per observed packet.
type TrackUnwrapper struct {
	seq *Unwrapper
	ts  *Unwrapper
}

// NewTrackUnwrapper creates a 16-bit sequence / 32-bit timestamp unwrapper.
func NewTrackUnwrapper() *TrackUnwrapper {
	return &TrackUnwrapper{seq: NewUnwrapper(16), ts: NewUnwrapper(32)}
}

// Observe expands one RTP packet's counters. On failure the unwrapper is
// left untouched (no partial advance), so a rejected packet cannot corrupt
// subsequent continuity decisions.
func (t *TrackUnwrapper) Observe(pkt *rtp.Packet) (seq, ts int64, err error) {
	seq, err = t.seq.Observe(uint64(pkt.SequenceNumber))
	if err != nil {
		return 0, 0, err
	}
	ts, err = t.ts.Observe(uint64(pkt.Timestamp))
	if err != nil {
		return 0, 0, err
	}
	return seq, ts, nil
}

// SeqObserved reports how many packets have been accepted.
func (t *TrackUnwrapper) SeqObserved() int64 { return t.seq.Observed() }
