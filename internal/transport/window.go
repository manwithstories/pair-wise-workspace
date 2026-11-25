package transport

import (
	"fmt"

	"github.com/pion/rtp"

	"archivectl/internal/timeline"
)

// Gap is an inclusive extended-sequence interval that was absent.
type Gap struct {
	Start  int64  `json:"start"`
	End    int64  `json:"end"`
	Count  int    `json:"count"`
	Reason string `json:"reason"`
}

// Counters contains the acceptance-visible transport statistics.
type Counters struct {
	Packets         int `json:"packets"`
	Delivered       int `json:"delivered"`
	DupDrop         int `json:"dup_drop"`
	Reordered       int `json:"reordered"`
	GapPackets      int `json:"gaps"`
	GapRanges       int `json:"gap_ranges"`
	WindowOverflows int `json:"window_overflow_events"`
}

// OrderedPacket retains both the parsed packet and extended wrapping values.
type OrderedPacket struct {
	Packet            rtp.Packet
	Raw               []byte
	ExtendedSequence  int64
	ExtendedTimestamp int64
	ArrivalIndex      int
}

// ReorderWindow deduplicates and reorders packets within a fixed span. It never
// allocates an unbounded pending map: sequence numbers farther than Size from
// the expected edge cause explicit gaps and window advancement.
type ReorderWindow struct {
	Size int

	sequence  timeline.WrapState
	timestamp timeline.WrapState
	next      int64
	started   bool
	buf       map[int64]OrderedPacket
	seen      map[int64]struct{}

	MaxExtendedSequence int64
	Gaps                []Gap
	Counters            Counters
}

func NewReorderWindow(size int) (*ReorderWindow, error) {
	if size <= 0 || size > 4096 {
		return nil, timeline.New(timeline.CodeInvalidArgument, "new reorder window",
			fmt.Sprintf("window %d outside 1..4096", size))
	}
	return &ReorderWindow{
		Size:      size,
		sequence:  timeline.WrapState{Bits: 16},
		timestamp: timeline.WrapState{Bits: 32},
		buf:       make(map[int64]OrderedPacket),
		seen:      make(map[int64]struct{}),
	}, nil
}

// AcceptBytes parses and feeds one arrival-order RTP datagram.
func (w *ReorderWindow) AcceptBytes(raw []byte) ([]OrderedPacket, error) {
	p, err := ParseRTP(raw)
	if err != nil {
		return nil, err
	}
	return w.Accept(p, raw)
}

// Accept feeds a parsed RTP packet and returns any newly contiguous packets.
func (w *ReorderWindow) Accept(p *rtp.Packet, raw []byte) ([]OrderedPacket, error) {
	w.Counters.Packets++
	previousHigh := w.MaxExtendedSequence
	extSeq, err := w.sequence.Extend(uint64(p.SequenceNumber))
	if err != nil {
		return nil, err
	}
	extTS, err := w.timestamp.Extend(uint64(p.Timestamp))
	if err != nil {
		return nil, err
	}
	w.MaxExtendedSequence = w.sequence.Max

	if !w.started {
		w.started = true
		w.next = extSeq
	}
	if _, dup := w.seen[extSeq]; dup || extSeq < w.next {
		w.Counters.DupDrop++
		w.seen[extSeq] = struct{}{}
		return nil, nil
	}

	if extSeq < previousHigh {
		// A missing-before-newer hole is reordering; sequential packets after a
		// lost burst still have extSeq > previousHigh and are not counted.
		w.Counters.Reordered++
	}

	ready := make([]OrderedPacket, 0)
	// A sequence outside the bounded span is a window overflow. Advance the
	// expected edge, deliver buffered packets we are forced to pass, and ledger
	// only sequences that were actually absent. This never invents media.
	if extSeq-int64(w.Size) >= w.next {
		w.Counters.WindowOverflows++
		target := extSeq - int64(w.Size) + 1
		for w.next < target {
			if buffered, ok := w.buf[w.next]; ok {
				ready = append(ready, buffered)
				delete(w.buf, w.next)
				w.Counters.Delivered++
			} else {
				w.recordGap(w.next, "window-overflow")
			}
			w.next++
		}
	}

	w.seen[extSeq] = struct{}{}
	w.evictReplay(extSeq)
	op := OrderedPacket{
		Packet:            *p,
		Raw:               raw,
		ExtendedSequence:  extSeq,
		ExtendedTimestamp: extTS,
		ArrivalIndex:      w.Counters.Packets - 1,
	}
	w.buf[extSeq] = op
	ready = append(ready, w.flushContiguous()...)
	return ready, nil
}

func (w *ReorderWindow) flushContiguous() []OrderedPacket {
	out := make([]OrderedPacket, 0)
	for {
		p, ok := w.buf[w.next]
		if !ok {
			break
		}
		out = append(out, p)
		delete(w.buf, w.next)
		w.Counters.Delivered++
		w.next++
	}
	return out
}

// Finish records all remaining holes up to the high-water mark and emits the
// received packets after those holes in sequence order.
func (w *ReorderWindow) Finish() []OrderedPacket {
	if !w.started {
		return nil
	}
	out := make([]OrderedPacket, 0)
	last := w.sequence.Max
	for seq := w.next; seq <= last; seq++ {
		if p, ok := w.buf[seq]; ok {
			out = append(out, p)
			delete(w.buf, seq)
			w.Counters.Delivered++
		} else {
			w.recordGap(seq, "missing-packet")
		}
		w.next = seq + 1
	}
	w.buf = make(map[int64]OrderedPacket)
	return out
}

func (w *ReorderWindow) recordGap(seq int64, reason string) {
	w.Counters.GapPackets++
	if len(w.Gaps) > 0 {
		g := &w.Gaps[len(w.Gaps)-1]
		if g.Reason == reason && g.End+1 == seq {
			g.End = seq
			g.Count++
			return
		}
	}
	w.Gaps = append(w.Gaps, Gap{Start: seq, End: seq, Count: 1, Reason: reason})
	w.Counters.GapRanges = len(w.Gaps)
}

func (w *ReorderWindow) evictReplay(high int64) {
	cutoff := high - int64(w.Size)*2
	for seq := range w.seen {
		if seq < cutoff {
			delete(w.seen, seq)
		}
	}
}

// HasGaps reports whether the capture is a partial recovery rather than a
// complete archive.
func (w *ReorderWindow) HasGaps() bool { return w.Counters.GapPackets > 0 }
