package rtp

// SeqExtender maps 16-bit RTP sequence numbers onto a monotonic 64-bit
// extended sequence space, following the wrap-detection approach of
// RFC 3550 appendix A.1. It is safe for exactly one packet stream
// (one track) fed in arrival order.
type SeqExtender struct {
	started bool
	maxSeq  uint16 // highest sequence number seen so far
	cycles  uint64 // number of times the 16-bit space wrapped
}

// Extend converts a 16-bit sequence number to its extended form.
// Packets arriving ahead of the current maximum (mod 2^16, within half
// the sequence space) advance the maximum; packets behind it are mapped
// to the cycle they belong to without moving the maximum.
func (e *SeqExtender) Extend(seq uint16) uint64 {
	if !e.started {
		e.started = true
		e.maxSeq = seq
		return uint64(seq)
	}
	maxExt := e.cycles<<16 | uint64(e.maxSeq)
	dist := uint16(seq - e.maxSeq) // modular forward distance
	if dist == 0 {
		return maxExt // exact duplicate of the current maximum
	}
	if dist < 0x8000 {
		// Ahead of the maximum. A numerically smaller value means the
		// 16-bit counter wrapped.
		if seq < e.maxSeq {
			e.cycles++
		}
		e.maxSeq = seq
		return e.cycles<<16 | uint64(seq)
	}
	// Behind the maximum: a late or reordered packet.
	back := uint64(uint16(e.maxSeq - seq))
	if back > maxExt {
		// Claims to predate the stream start; clamp to the origin.
		return 0
	}
	return maxExt - back
}

// MaxExtended returns the highest extended sequence number seen so far.
// The second return value is false before the first Extend call.
func (e *SeqExtender) MaxExtended() (uint64, bool) {
	if !e.started {
		return 0, false
	}
	return e.cycles<<16 | uint64(e.maxSeq), true
}
