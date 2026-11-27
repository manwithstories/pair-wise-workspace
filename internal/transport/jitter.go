package transport

// Gap is a timeline hole left by packets that could not be recovered
// before they left the jitter window.
type Gap struct {
	StartSeq uint16
	EndSeq   uint16
	PTS      int64 // unified nanosecond timestamp of the first missing packet
	Count    int
}

// JitterConfig bounds buffer memory. Only packets within Window sequence
// numbers of the playout head are acted upon, so memory scales with the
// window, not the source length.
type JitterConfig struct {
	Window    int // max out-of-order spread tolerated
	NACKDepth int // packets retained for retransmission requests
}

// DefaultJitterConfig is tuned for the synthetic loss bursts.
func DefaultJitterConfig() JitterConfig {
	return JitterConfig{Window: 32, NACKDepth: 64}
}

// JitterBuffer reorders and deduplicates impaired packets keyed by their RTP
// sequence number. It holds only the distinct packets of one recovery pass;
// the engine drains entries as it walks the sequence space, so its size is
// bounded by the jitter window rather than the stream's total length.
type JitterBuffer struct {
	present  map[uint16]*Packet
	received map[uint16]bool // dedup guard for retransmitted/duplicate packets
}

// NewJitterBuffer constructs a buffer.
func NewJitterBuffer(_ JitterConfig) *JitterBuffer {
	return &JitterBuffer{
		present:  make(map[uint16]*Packet),
		received: make(map[uint16]bool),
	}
}

// Push accepts one impaired packet. Duplicates (same sequence seen twice)
// are ignored; every distinct packet is buffered.
func (j *JitterBuffer) Push(p *Packet) bool {
	seq := p.RTP.SequenceNumber
	if j.received[seq] {
		return false
	}
	j.received[seq] = true
	j.present[seq] = p
	return true
}

// Has reports whether a sequence number has ever been observed on the wire.
func (j *JitterBuffer) Has(seq uint16) bool { return j.received[seq] }

// seqDist returns the signed forward distance from a to b in 16-bit space.
func seqDist(a, b uint16) int {
	return int(int16(b - a))
}
