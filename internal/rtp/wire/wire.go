// Package wire adapts raw RTP datagrams to the reorderer. pion/rtp parses
// the header and 16-bit sequence number; this package extends that number
// past wrap and keeps timestamps on the unified nanosecond axis.
package wire

import (
	"fmt"

	"github.com/pion/rtp"

	"mediarun/internal/clock"
)

// Extender turns a 16-bit wire sequence into a monotonic 64-bit one, the
// inverse of RTP's wrap behaviour. It is deliberately simple: two observed
// sequences more than half the space apart mean a wrap forward, anything
// else backwards is a reorder or duplicate and keeps its current epoch.
type Extender struct {
	epoch     uint64
	last      uint16
	started   bool
	WrapCount int
}

// Extend maps a wire sequence to an extended sequence.
func (e *Extender) Extend(seq uint16) uint64 {
	if !e.started {
		e.started = true
		e.last = seq
		return e.epoch<<16 | uint64(seq)
	}
	dist := int64(seq) - int64(e.last)
	if dist > 32767 {
		// Backwards by more than half the sequence space: a later epoch's
		// sequence arrived before this epoch finished.
		e.epoch--
	} else if dist < -32767 {
		e.epoch++
		e.WrapCount++
	}
	e.last = seq
	return e.epoch<<16 | uint64(seq)
}

// ParsePacket decodes one datagram with pion/rtp, returning the header
// fields the recovery stages need. The payload is intentionally not copied.
func ParsePacket(datagram []byte) (seq uint16, timestamp uint32, ssrc uint32, err error) {
	var p rtp.Packet
	if err := p.Unmarshal(datagram); err != nil {
		return 0, 0, 0, clock.Wrap(clock.KindInvalid, clock.CodeUnsupported,
			"rtp", fmt.Errorf("malformed datagram: %w", err))
	}
	return p.SequenceNumber, p.Timestamp, p.SSRC, nil
}

// StampToNS converts a 32-bit RTP timestamp at the given clock rate onto the
// nanosecond axis. A rate of zero is rejected rather than guessed.
func StampToNS(timestamp uint32, clockRate uint32) (int64, error) {
	if clockRate == 0 {
		return 0, clock.New(clock.KindInvalid, clock.CodeInvalidDuration,
			"rtp", "zero clock rate")
	}
	return int64(timestamp) * 1_000_000_000 / int64(clockRate), nil
}
