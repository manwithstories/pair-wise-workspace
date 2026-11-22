// Package ingest receives RTP packets, validates headers,
// de-duplicates and reorders them within a bounded window, and reports
// defects through the shared error model. Late packets arriving outside
// the reorder window are recorded as defects, never silently applied.
package ingest

import (
	"fmt"

	"github.com/pion/rtp"

	"mediasync/internal/defect"
)

const (
	// Window is the reorder window size in packets.
	Window = 1024
	// Track names the packet track in defect records.
	Track = "rtp"
	// maxReorderGap bounds how far ahead of the playout cursor a packet
	// may be before its arrival is attributed to loss instead of
	// reordering.
	maxReorderGap = 16
)

// Encode wraps one sample record into an RTP datagram.
func Encode(seq uint16, ts uint32, ssrc uint32, payload []byte) ([]byte, error) {
	p := rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           ssrc,
		},
		Payload: payload,
	}
	return p.Marshal()
}

// Reassembler de-duplicates and reorders packets by sequence number,
// tolerating 16-bit wraparound. Memory is bounded by the window.
type Reassembler struct {
	loss    *LossTracker
	next    uint32 // next expected extended sequence
	started bool
	maxExt  uint32
	buf     map[uint32][]byte
	seen    map[uint32]struct{}
	// Defects accumulates every transport defect in arrival order.
	Defects []defect.Defect
}

// NewReassembler returns a Reassembler reporting losses to loss.
func NewReassembler(loss *LossTracker) *Reassembler {
	return &Reassembler{loss: loss, buf: map[uint32][]byte{}, seen: map[uint32]struct{}{}}
}

// Push consumes one raw RTP datagram and returns any payloads that
// became contiguously available, in sequence order.
func (r *Reassembler) Push(raw []byte) ([][]byte, error) {
	var p rtp.Packet
	if err := p.Unmarshal(raw); err != nil {
		return nil, fmt.Errorf("ingest: bad RTP datagram: %w", err)
	}
	return r.pushSeq(p.SequenceNumber, p.Payload), nil
}

func (r *Reassembler) pushSeq(seq uint16, payload []byte) [][]byte {
	ext := r.extend(seq)
	if !r.started {
		r.started, r.next, r.maxExt = true, ext, ext
	}
	if _, dup := r.seen[ext]; dup {
		r.loss.AddDup()
		r.Defects = append(r.Defects, defect.New(Track, int64(ext), -1, defect.Duplicate, "duplicate packet"))
		return nil
	}
	if ext < r.next {
		r.loss.AddLate()
		r.seen[ext] = struct{}{}
		r.Defects = append(r.Defects, defect.New(Track, int64(ext), -1, defect.LatePacket,
			"late packet outside reorder window"))
		return nil
	}
	r.seen[ext] = struct{}{}
	r.buf[ext] = payload
	if ext > r.maxExt {
		r.maxExt = ext
	}
	if gap := ext - r.next; gap > 0 && gap < maxReorderGap {
		r.Defects = append(r.Defects, defect.New(Track, int64(ext), -1, defect.Reordered,
			fmt.Sprintf("arrived %d positions early", gap)))
	}
	// Slide the window: anything too far behind is declared lost.
	for r.maxExt >= r.next+Window {
		r.declareMissing(r.next)
		r.next++
	}
	out := r.drain()
	r.evict()
	return out
}

// Finish declares every still-open gap lost and flushes the buffer in
// sequence order.
func (r *Reassembler) Finish() [][]byte {
	if !r.started {
		return nil
	}
	var out [][]byte
	for s := r.next; s <= r.maxExt; s++ {
		if p, ok := r.buf[s]; ok {
			out = append(out, p)
			delete(r.buf, s)
		} else {
			r.declareMissing(s)
		}
	}
	r.next = r.maxExt + 1
	return out
}

func (r *Reassembler) drain() [][]byte {
	var out [][]byte
	for {
		p, ok := r.buf[r.next]
		if !ok {
			return out
		}
		delete(r.buf, r.next)
		out = append(out, p)
		r.next++
	}
}

func (r *Reassembler) declareMissing(s uint32) {
	if _, ok := r.buf[s]; ok {
		return
	}
	r.loss.AddMissing(s)
	r.Defects = append(r.Defects, defect.New(Track, int64(s), -1, defect.Loss, "packet lost"))
}

// extend maps a 16-bit sequence number onto the extended 32-bit
// sequence space, tolerating wraparound.
func (r *Reassembler) extend(seq uint16) uint32 {
	if !r.started {
		return uint32(seq)
	}
	ext := (r.maxExt & 0xffff0000) | uint32(seq)
	if ext+32768 < r.maxExt {
		ext += 1 << 16
	} else if ext > r.maxExt+32768 && ext >= 1<<16 {
		ext -= 1 << 16
	}
	return ext
}

// evict keeps the seen-set bounded by the window.
func (r *Reassembler) evict() {
	if len(r.seen) <= 2*Window {
		return
	}
	for s := range r.seen {
		if s+Window < r.next {
			delete(r.seen, s)
		}
	}
}
