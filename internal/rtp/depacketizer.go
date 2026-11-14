// Package rtp reorders RTP packets, drops duplicates, handles 16-bit sequence
// wrap-around and reassembles application frames at marker/timestamp edges.
package rtp

import (
	"sync"

	pionrtp "github.com/pion/rtp"
)

// Frame is one reassembled application frame.
type Frame struct {
	Index       int    `json:"frame_index"`
	FirstSeq    uint16 `json:"first_seq"`
	LastSeq     uint16 `json:"last_seq"`
	Timestamp   uint32 `json:"timestamp"`
	Bytes       int    `json:"bytes"`
	SHA256      string `json:"sha256"`
	PacketCount int    `json:"packet_count"`
	Payload     []byte `json:"-"`
}

// Stats are the packet-level counters produced while reassembling.
type Stats struct {
	Received        int           `json:"rtp_received"`
	Duplicates      int           `json:"rtp_duplicates"`
	Reordered       int           `json:"rtp_reordered"`
	MaxReorderDepth int           `json:"max_reorder_depth"`
	GapSegments     int           `json:"seq_gap_segments"`
	GapPackets      int           `json:"seq_gap_packets"`
	Frames          int           `json:"frames_reassembled"`
	Bytes           int64         `json:"bytes"`
	FirstSeq        uint16        `json:"first_seq"`
	LastSeq         uint16        `json:"last_seq"`
	Gaps            []GapInterval `json:"-"`
	HaveBounds      bool          `json:"-"`
}

// GapInterval is one contiguous missing-sequence interval.
type GapInterval struct {
	First uint16 `json:"first"`
	Last  uint16 `json:"last"`
	Count int    `json:"count"`
}

// Depacketizer reassembles ordered frames from a lossy, reordered, possibly
// duplicated RTP stream. A bounded reorder window holds late packets; when the
// backlog behind a missing frontier exceeds the window, the missing range is
// declared a permanent gap and skipped, capping memory usage.
type Depacketizer struct {
	mu       sync.Mutex
	window   int
	pending  map[uint16]*pionrtp.Packet
	lateSeqs map[uint16]bool // sequences of packets that arrived late
	next     uint16          // next in-order sequence expected
	maxExt   uint32          // highest extended sequence ever accepted
	missing  int             // sequences skipped past the stalled frontier awaiting loss
	started  bool
	basePos  uint32 // extended seq of first accepted packet
	stats    Stats

	curTimestamp uint32
	curPayload   []byte
	curFirst     uint16
	curLast      uint16
	curPackets   int
	frameIndex   int
}

// NewDepacketizer creates a depacketizer with the given reorder window.
func NewDepacketizer(window int) *Depacketizer {
	if window <= 0 {
		window = 64
	}
	return &Depacketizer{window: window, pending: map[uint16]*pionrtp.Packet{}, lateSeqs: map[uint16]bool{}}
}

// extend maps a 16-bit sequence to an extended 32-bit sequence relative to the
// first packet, handling wrap-around at the 65536 boundary.
func extend(base uint32, seq uint16) uint32 {
	delta := int32(int16(seq - uint16(base)))
	return base + uint32(delta)
}

// Push ingests one raw RTP packet, returning any frames completed in order.
func (d *Depacketizer) Push(raw []byte) ([]Frame, error) {
	var pkt pionrtp.Packet
	if err := pkt.Unmarshal(raw); err != nil {
		return nil, err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	d.stats.Received++
	seq := pkt.SequenceNumber
	if !d.stats.HaveBounds {
		d.stats.FirstSeq, d.stats.LastSeq = seq, seq
		d.stats.HaveBounds = true
	} else if extend(d.basePos, seq) > extend(d.basePos, d.stats.LastSeq) {
		d.stats.LastSeq = seq
	}

	if !d.started {
		d.started = true
		d.basePos = uint32(seq)
		d.next, d.maxExt = seq, uint32(seq)
	}

	pktExt := extend(d.basePos, seq)
	nextExt := extend(d.basePos, d.next)

	if _, seen := d.pending[seq]; seen {
		d.stats.Duplicates++ // same sequence already buffered
		return nil, nil
	}
	if pktExt < nextExt {
		// Frontier already passed this sequence. If it belongs to a recorded
		// gap it is a late (reordered) fill; otherwise it is a true duplicate.
		if d.insideGap(pktExt) {
			d.stats.Reordered++
		} else {
			d.stats.Duplicates++
		}
		return nil, nil
	}

	// A packet arriving while a higher sequence has already been seen is
	// genuinely out of order, whether or not a gap sits at the frontier.
	wasBelowHighWater := pktExt < d.maxExt
	if pktExt > d.maxExt {
		d.maxExt = pktExt
	}
	if wasBelowHighWater {
		d.stats.Reordered++
		d.lateSeqs[seq] = true
		// Reorder depth is the displacement distance: how many sequence
		// positions this late packet sits behind the highest accepted seq.
		if lag := int(d.maxExt - pktExt); lag > d.stats.MaxReorderDepth {
			d.stats.MaxReorderDepth = lag
		}
	}
	d.pending[seq] = pkt.Clone()
	d.enforceWindow()

	var out []Frame
	for {
		p, ok := d.pending[d.next]
		if !ok {
			break
		}
		delete(d.pending, d.next)
		delete(d.lateSeqs, d.next)
		out = append(out, d.deliver(p)...)
		d.next++
	}
	return out, nil
}

// enforceWindow keeps memory bounded: when more than window packets wait on a
// missing frontier, permanently skip the smallest unresolved gap.
func (d *Depacketizer) enforceWindow() {
	// Memory is bounded by the total backlog behind a missing frontier
	// (loss wait plus genuinely late packets); the reorder depth metric is
	// tracked separately so loss never inflates it.
	for d.bufferedReorderDepth() > d.window {
		if _, ok := d.pending[d.next]; ok {
			return
		}
		lowest, have := d.lowestBufferedExt()
		if !have {
			return
		}
		nextExt := extend(d.basePos, d.next)
		skipped := int(lowest - nextExt)
		d.recordGap(d.next, uint16(lowest-1), skipped)
		d.missing += skipped
		d.next = uint16(lowest)
	}
}

// lowestBufferedExt returns the smallest extended sequence currently buffered.
func (d *Depacketizer) lowestBufferedExt() (uint32, bool) {
	lowest, have := uint32(0), false
	for seq := range d.pending {
		ext := extend(d.basePos, seq)
		if !have || ext < lowest {
			lowest, have = ext, true
		}
	}
	return lowest, have
}

func (d *Depacketizer) insideGap(ext uint32) bool {
	for _, g := range d.stats.Gaps {
		lo := extend(d.basePos, g.First)
		hi := extend(d.basePos, g.Last)
		if ext >= lo && ext <= hi {
			return true
		}
	}
	return false
}

// Flush emits any partial frame still open at stream end.
func (d *Depacketizer) Flush() *Frame {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.curPackets == 0 {
		return nil
	}
	fr := d.flushFrame()
	return &fr
}

// Finalize closes remaining open gaps and returns finalized stats/gaps.
func (d *Depacketizer) Finalize() (Stats, []GapInterval) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.finalizeGaps()
}

// NextSeq returns the next contiguous sequence expected (for resume).
func (d *Depacketizer) NextSeq() uint16 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.next
}

// Reached reports whether the given extended logical position has been seen.
func (d *Depacketizer) Reached(logicalPos uint32) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.started {
		return false
	}
	return d.maxExt >= d.basePos+logicalPos
}

// Stats returns a copy of the current counters.
func (d *Depacketizer) Stats() Stats {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stats
}

// Gaps returns recorded gap intervals.
func (d *Depacketizer) Gaps() []GapInterval {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]GapInterval(nil), d.stats.Gaps...)
}

// SetFrameOffset shifts subsequently produced frame indices (for resume).
func (d *Depacketizer) SetFrameOffset(offset int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.frameIndex = offset
}
