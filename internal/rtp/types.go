// Package rtpx maintains per-SSRC reception statistics: jitter estimation,
// out-of-order handling inside a bounded window, loss and duplicate counting,
// and 16-bit sequence number wraparound.
package rtpx

import (
	"errors"
	"time"
)

// ErrNoPackets is returned when a summary is requested before any packet
// arrived, so an empty run is never reported as a zero-jitter measurement.
var ErrNoPackets = errors.New("no RTP packets observed")

const (
	// DefaultReorderWindow is the default number of sequence numbers a packet
	// may be late by and still be classified as reordered rather than a
	// late arrival outside the window.
	DefaultReorderWindow = 64

	// jitterGainDivisor is the RFC 3550 smoothing gain: J += (|D| - J) / 16.
	jitterGainDivisor = 16.0

	seqModulus = 1 << 16
	seqHalf    = 1 << 15
)

// Config tunes the stream analyser.
type Config struct {
	// ClockRate is the codec clock in Hz (90000 for video, 48000 for opus).
	ClockRate int
	// ReorderWindow is the maximum late distance counted as reordering.
	ReorderWindow int
}

// Packet is the reception record the analyser consumes. Timestamp and Arrival
// are paired to derive the transit-time variation jitter is computed from, so
// Timestamp must be the sender's RTP timestamp.
type Packet struct {
	SSRC      uint32
	Sequence  uint16
	Timestamp uint32
	Marker    bool
	Payload   int
	Arrival   time.Time
}

// StreamStats is the per-SSRC snapshot.
type StreamStats struct {
	SSRC          uint32  `json:"ssrc"`
	Received      uint64  `json:"received"`
	Unique        uint64  `json:"unique_packets"`
	Expected      int64   `json:"expected"`
	Lost          int64   `json:"lost"`
	Duplicates    uint64  `json:"duplicates"`
	Reordered     uint64  `json:"reordered"`
	LateOutside   uint64  `json:"late_outside_window"`
	FirstSequence uint16  `json:"first_sequence"`
	LastSequence  uint16  `json:"last_sequence"`
	WrapCount     uint64  `json:"wrap_count"`
	MarkerCount   uint64  `json:"marker_count"`
	PayloadBytes  uint64  `json:"payload_bytes"`
	JitterAvgMs   float64 `json:"jitter_avg_ms"`
	JitterMaxMs   float64 `json:"jitter_max_ms"`
}

// Summary aggregates every SSRC seen by one analyser.
type Summary struct {
	Streams       []StreamStats `json:"streams"`
	Received      uint64        `json:"received_packets"`
	Unique        uint64        `json:"unique_packets"`
	Lost          int64         `json:"lost_packets"`
	Duplicates    uint64        `json:"duplicate_packets"`
	Reordered     uint64        `json:"reordered_packets"`
	LateOutside   uint64        `json:"late_outside_window"`
	JitterAvgMs   float64       `json:"jitter_avg_ms"`
	JitterMaxMs   float64       `json:"jitter_max_ms"`
	ClockRate     int           `json:"clock_rate"`
	ReorderWindow int           `json:"reorder_window"`
}

// streamState is one SSRC's mutable state. The Analyzer mutex guards it, so
// the fields need no locks of their own.
type streamState struct {
	cfg Config

	started bool
	// firstSeq and highest are extended (monotonic) counters, not the 16-bit
	// wire values; this is what makes wraparound arithmetic correct.
	firstSeq uint64
	highest  uint64

	lastArr   time.Time
	lastTS    uint32
	haveTrans bool

	received   uint64
	unique     uint64
	duplicates uint64
	reordered  uint64
	late       uint64
	wraps      uint64
	markers    uint64
	payload    uint64

	jitter    float64
	jitterMax float64

	// seen holds recently observed extended sequence numbers. It is pruned to
	// a small multiple of the reorder window, so memory is bounded by the
	// window rather than by stream length.
	seen map[uint64]struct{}
}
