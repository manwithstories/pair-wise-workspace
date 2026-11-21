package stream

import (
	"encoding/binary"
	"io"

	"github.com/pion/rtp"

	"mediacheck/internal/fault"
)

// DefaultWindow is the per-stream reorder window in packets.
const DefaultWindow = 16

// StreamStats summarizes one RTP stream (one SSRC).
type StreamStats struct {
	SSRC      uint32 `json:"ssrc"`
	Received  int    `json:"received"`
	Committed int    `json:"committed"` // delivered in order
	Reordered int    `json:"reordered"` // arrived out of order, recovered
	Late      int    `json:"late"`      // outside window, counted as gap
	Gaps      int    `json:"gaps"`      // sequence numbers never delivered
}

type streamState struct {
	init    bool
	next    uint16 // next expected sequence number
	pending map[uint16]*rtp.Packet
	stats   *StreamStats
}

// Reorderer maintains one reorder window per SSRC. Packets inside the
// window are buffered until they can be committed in sequence order;
// packets older than the committed edge are counted as gaps and never
// rewrite already-committed order.
type Reorderer struct {
	window  int
	streams map[uint32]*streamState
	stats   []*StreamStats
}

// NewReorderer creates a Reorderer with the given window (clamped 1..256).
func NewReorderer(window int) *Reorderer {
	if window < 1 {
		window = 1
	}
	if window > 256 {
		window = 256
	}
	return &Reorderer{window: window, streams: map[uint32]*streamState{}}
}

// Stats returns per-stream statistics in first-seen order.
func (r *Reorderer) Stats() []*StreamStats { return r.stats }

// Feed ingests one packet and returns any packets that become
// committable, in sequence order.
func (r *Reorderer) Feed(p *rtp.Packet) []*rtp.Packet {
	ss := r.streams[p.SSRC]
	if ss == nil {
		ss = &streamState{pending: map[uint16]*rtp.Packet{},
			stats: &StreamStats{SSRC: p.SSRC}}
		r.streams[p.SSRC] = ss
		r.stats = append(r.stats, ss.stats)
	}
	st := ss.stats
	st.Received++
	if !ss.init {
		ss.init = true
		ss.next = p.SequenceNumber
	}
	diff := int16(p.SequenceNumber - ss.next)
	if diff < 0 {
		// Older than the committed edge: too late, count as gap.
		st.Late++
		st.Gaps++
		return nil
	}
	if int(diff) >= r.window {
		// Push the window forward: commit buffered packets up to the
		// new one, counting the truly missing sequence numbers as gaps.
		var out []*rtp.Packet
		for ss.next != p.SequenceNumber {
			if q, ok := ss.pending[ss.next]; ok {
				delete(ss.pending, ss.next)
				st.Committed++
				out = append(out, q)
			} else {
				st.Gaps++
			}
			ss.next++
		}
		ss.pending[p.SequenceNumber] = p
		return append(out, r.commit(ss)...)
	}
	if _, dup := ss.pending[p.SequenceNumber]; dup {
		return nil
	}
	ss.pending[p.SequenceNumber] = p
	if p.SequenceNumber != ss.next {
		st.Reordered++
		return nil
	}
	return r.commit(ss)
}

func (r *Reorderer) commit(ss *streamState) []*rtp.Packet {
	var out []*rtp.Packet
	for {
		p, ok := ss.pending[ss.next]
		if !ok {
			break
		}
		delete(ss.pending, ss.next)
		ss.stats.Committed++
		ss.next++
		out = append(out, p)
	}
	return out
}

// Flush ends the stream: uncommitted pending packets become gaps.
func (r *Reorderer) Flush() {
	for _, ss := range r.streams {
		ss.stats.Gaps += len(ss.pending)
		ss.pending = map[uint16]*rtp.Packet{}
	}
}

// ParseRTPStream decodes length-prefixed RTP packets (2-byte big-endian
// length + packet bytes, our self-generated sample format) and feeds
// them to the Reorderer. Malformed packets are recoverable faults
// located by packet ordinal.
func ParseRTPStream(data []byte, rec *Reorderer, fl *fault.List) {
	ord := 0
	for len(data) >= 2 {
		n := int(binary.BigEndian.Uint16(data))
		data = data[2:]
		if n == 0 || n > len(data) {
			fl.Addf(fault.CatRTP, int64(ord), true,
				"truncated RTP packet: want %d bytes, have %d", n, len(data))
			return
		}
		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(data[:n]); err != nil {
			fl.Addf(fault.CatRTP, int64(ord), true, "bad RTP packet: %v", err)
		} else {
			rec.Feed(pkt)
		}
		data = data[n:]
		ord++
	}
	if len(data) > 0 {
		fl.Addf(fault.CatRTP, int64(ord), true,
			"%d trailing bytes after last RTP packet", len(data))
	}
	rec.Flush()
}

var _ = io.EOF
