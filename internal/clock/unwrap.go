package clock

import (
	"fmt"
	"math"

	"github.com/pion/rtp"
)

// RTPTick is the result of extending one 32-bit RTP timestamp onto the
// node's unwrapped 64-bit tick axis.
type RTPTick struct {
	Sequence   uint16 `json:"sequence"`
	Raw        uint32 `json:"raw_timestamp"`
	Extended   int64  `json:"extended_timestamp"`
	Duplicate  bool   `json:"duplicate"`
	Backward   bool   `json:"backward"`
	Wrap       bool   `json:"wrap"`
	Jump       bool   `json:"jump"`
	Disorder   bool   `json:"disorder"`
	AdvanceTks int64  `json:"advance_ticks"`
}

// Unwrapper tracks a wrapping 32-bit RTP timestamp counter. It never
// changes the source value: every push keeps Raw and reports any
// ambiguity through flags.
type Unwrapper struct {
	initialized bool
	lastRaw     uint32
	cycles      int64
	lastExt     int64
	haveSeq     bool
	lastSeq     uint16
	// jumpThreshold is the largest forward advance, in ticks, treated
	// as normal progression. Larger steps are flagged as jumps.
	jumpThreshold int64
}

// NewUnwrapper creates an unwrapper. jumpThreshold <= 0 disables jump
// classification while keeping wrap and duplicate detection.
func NewUnwrapper(jumpThreshold int64) *Unwrapper {
	return &Unwrapper{jumpThreshold: jumpThreshold}
}

// Push extends one RTP header timestamp.
func (u *Unwrapper) Push(h rtp.Header) (RTPTick, error) {
	raw := h.Timestamp
	t := RTPTick{Sequence: h.SequenceNumber, Raw: raw}

	if !u.initialized {
		u.initialized = true
		u.lastRaw = raw
		u.lastExt = int64(raw)
		u.haveSeq = true
		u.lastSeq = h.SequenceNumber
		t.Extended = u.lastExt
		return t, nil
	}

	if u.haveSeq {
		seqDelta := int32(int16(h.SequenceNumber - u.lastSeq))
		if seqDelta < 0 {
			t.Disorder = true
		}
	}
	u.lastSeq = h.SequenceNumber
	u.haveSeq = true

	delta := int64(int32(raw - u.lastRaw))
	switch {
	case raw == u.lastRaw:
		t.Duplicate = true
		t.Extended = u.lastExt
		return t, nil
	case delta > 0 && raw < u.lastRaw:
		// Forward movement across the 2^32 boundary.
		u.cycles++
		t.Wrap = true
		t.Extended = u.cycles<<32 + int64(raw)
	case delta > 0:
		t.Extended = u.cycles<<32 + int64(raw)
	case delta < 0 && raw > u.lastRaw:
		// Backwards over the boundary. Roll the cycle count back so the
		// extended value stays on the same continuous axis.
		if u.cycles == 0 {
			return t, fmt.Errorf("timestamp underflow before first wrap: raw=%d last=%d", raw, u.lastRaw)
		}
		u.cycles--
		t.Wrap = true
		t.Extended = u.cycles<<32 + int64(raw)
	case delta < 0:
		t.Backward = true
		t.Extended = u.cycles<<32 + int64(raw)
	}

	t.AdvanceTks = t.Extended - u.lastExt
	if t.AdvanceTks < 0 {
		t.Backward = true
	}
	if u.jumpThreshold > 0 && t.AdvanceTks > u.jumpThreshold {
		t.Jump = true
	}
	u.lastRaw = raw
	u.lastExt = t.Extended
	return t, nil
}

// LastExtended returns the most recent extended timestamp.
func (u *Unwrapper) LastExtended() int64 { return u.lastExt }

// Wraps reports how many 2^32 boundaries have been crossed.
func (u *Unwrapper) Wraps() int64 { return u.cycles }

// UnwrapRTP extends a plain sequence of 32-bit timestamps without
// sequence-number context. It is used by tests and by callers that
// only retained timestamp columns. Duplicate and backward events are
// reported, never repaired.
type UnwrapRTP struct {
	initialized bool
	lastRaw     uint32
	cycles      int64
}

// Push extends one raw timestamp.
func (u *UnwrapRTP) Push(raw uint32) int64 {
	if !u.initialized {
		u.initialized = true
		u.lastRaw = raw
		return int64(raw)
	}
	if raw == u.lastRaw {
		return u.cycles<<32 + int64(raw)
	}
	if int64(int32(raw-u.lastRaw)) > 0 && raw < u.lastRaw {
		u.cycles++
	}
	if int64(int32(raw-u.lastRaw)) < 0 && raw > u.lastRaw && u.cycles > 0 {
		u.cycles--
	}
	u.lastRaw = raw
	return u.cycles<<32 + int64(raw)
}

// MaxRTPTimestamp is the wrap modulus minus one.
const MaxRTPTimestamp = uint32(math.MaxUint32)

// WrapAt is a convenience for fixtures: it maps an absolute extended
// tick count onto the 32-bit RTP wire counter.
func WrapAt(extended int64) uint32 { return uint32(extended & 0xFFFFFFFF) }
