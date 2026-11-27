// Package model defines the shared nanosecond time base, bounded resource
// limits and the fault/gap model used by every archive stage.
package model

// Nano is a point or duration on the shared, monotonic media timeline,
// measured in nanoseconds. All manifest and RTP timestamps are normalised
// onto this base so audio, video and playlist clocks can be compared.
type Nano int64

const (
	Nanosecond  Nano = 1
	Microsecond Nano = 1_000 * Nanosecond
	Millisecond Nano = 1_000 * Microsecond
	Second      Nano = 1_000 * Millisecond
)

// Bounded resource limits. Inputs that exceed these limits are rejected so
// that recovery work stays inside the declared memory and processing budget.
const (
	// MaxPacketBytes caps a single RTP packet (header + payload).
	MaxPacketBytes = 1400
	// MaxPayloadBytes caps the payload section of one RTP packet.
	MaxPayloadBytes = 1200
	// MaxQueueBytes is the default memory ceiling for a bounded queue.
	MaxQueueBytes = 8 * 1024 * 1024
	// MaxRepairWindow is the largest number of sequence numbers a repair
	// stage is allowed to look back. Retransmissions older than this are
	// refused rather than buffered indefinitely.
	MaxRepairWindow = 256
	// MaxSegmentSeconds rejects absurd manifest segment durations.
	MaxSegmentSeconds = 60
	// MaxSegments caps the number of segments in one playlist.
	MaxSegments = 4096
	// MaxConcurrency caps goroutine fan-out inside the pipeline.
	MaxConcurrency = 4
)

// Clock maps a wrapping RTP timestamp (per clock-rate, e.g. 90 kHz video or
// 48 kHz audio) onto the shared nanosecond timeline. The first observed
// timestamp anchors the clock; later values are unwrapped using signed
// modular differences, so 32-bit wrap-around is handled transparently.
type Clock struct {
	rate    uint32
	started bool
	baseRTP uint32
	extRTP  int64 // unwrapped units relative to the first observation
	last    uint32
}

// NewClock returns a clock for the given RTP clock rate. A zero rate is
// treated as an invalid configuration.
func NewClock(rate uint32) *Clock {
	return &Clock{rate: rate}
}

// Rate returns the RTP clock rate the clock was built with.
func (c *Clock) Rate() uint32 { return c.rate }

// ToNano converts one RTP timestamp to shared nanoseconds. Timestamps must
// be fed in arrival order; out-of-order packets still resolve correctly as
// long as their distance stays inside the 32-bit modular half range.
func (c *Clock) ToNano(ts uint32) Nano {
	if !c.started {
		c.started = true
		c.baseRTP = ts
		c.last = ts
		return 0
	}
	delta := int64(int32(ts - c.last))
	c.extRTP += delta
	c.last = ts
	return Nano(c.extRTP * int64(Second.Nanos()) / int64(c.rate))
}

// Nanos exposes the unit value of a Nano duration for arithmetic callers.
func (n Nano) Nanos() int64 { return int64(n) }

// Milliseconds returns the duration expressed in floating milliseconds.
func (n Nano) Milliseconds() float64 {
	return float64(n) / float64(Millisecond)
}

// SeqExt unwraps 16-bit RTP sequence numbers into a monotonic int64 stream.
// It uses the signed modular difference, which is correct while jumps stay
// within half of the 16-bit space (32768 packets), matching the RTP spec.
type SeqExt struct {
	started bool
	last    uint16
	ext     int64
}

// NewSeqExt returns an empty sequence unwrapper.
func NewSeqExt() *SeqExt { return &SeqExt{} }

// Extend feeds a raw sequence number and returns its unwrapped value.
func (s *SeqExt) Extend(seq uint16) int64 {
	if !s.started {
		s.started = true
		s.last = seq
		s.ext = int64(seq)
		return s.ext
	}
	s.ext += int64(int16(seq - s.last))
	s.last = seq
	return s.ext
}

// WrapDist returns the signed modular distance from a to b in 16-bit
// sequence space. A negative result means b is older than a.
func WrapDist(a, b uint16) int { return int(int16(b - a)) }
