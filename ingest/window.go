package ingest

import (
	"errors"
	"fmt"
	"sort"

	"medcheck/fault"
)

var ErrBackpressure = errors.New("ingest: reorder window full; apply backpressure")

// Source identifies one camera/stream sequence space.
type Source struct {
	Camera string `json:"camera"`
	Stream string `json:"stream"`
}

// Source returns the packet's sequence-space key.
func (h Header) Source() Source { return Source{Camera: h.Camera, Stream: h.Stream} }

// Stats contains arrival-normalization counters.
type Stats struct {
	Received     int `json:"received"`
	Duplicates   int `json:"duplicates"`
	Reordered    int `json:"reordered"`
	Delivered    int `json:"delivered"`
	Backpressure int `json:"backpressure"`
	BurstLost    int `json:"burst_lost"`
}

type sourceState struct {
	next        uint64
	highest     uint64
	highestSeen bool
	pending     map[uint64]Packet
}

// Window deduplicates and reorders one or more camera/stream sequence spaces.
type Window struct {
	maxPending int
	faults     *fault.Model
	stats      Stats
	sources    map[Source]*sourceState
}

// NewWindow creates a bounded reorder window. maxPending limits buffered
// out-of-order packets per source.
func NewWindow(maxPending int, faults *fault.Model) *Window {
	if maxPending < 1 {
		maxPending = 1
	}
	return &Window{maxPending: maxPending, faults: faults, sources: make(map[Source]*sourceState)}
}

// Stats returns a copy of window counters.
func (w *Window) Stats() Stats { return w.stats }

func (w *Window) state(src Source) *sourceState {
	st := w.sources[src]
	if st == nil {
		st = &sourceState{pending: make(map[uint64]Packet)}
		w.sources[src] = st
	}
	return st
}

// Ingest accepts one packet and returns any newly contiguous packets.
//
// Duplicates are dropped. An out-of-order packet is buffered until the window
// is full; a full window returns ErrBackpressure without retaining the packet.
func (w *Window) Ingest(p Packet) ([]Packet, error) {
	w.stats.Received++
	src := p.Source()
	st := w.state(src)

	if p.Seq < st.next {
		w.stats.Duplicates++
		w.faults.Add(fault.Fault{Kind: fault.DuplicatePacket, Camera: src.Camera, Stream: src.Stream,
			Span: fault.Span{StartSeq: p.Seq, EndSeq: p.Seq}, Message: "packet below contiguous frontier"})
		return nil, nil
	}
	if _, exists := st.pending[p.Seq]; exists {
		w.stats.Duplicates++
		w.faults.Add(fault.Fault{Kind: fault.DuplicatePacket, Camera: src.Camera, Stream: src.Stream,
			Span: fault.Span{StartSeq: p.Seq, EndSeq: p.Seq}, Message: "packet already in reorder window"})
		return nil, nil
	}
	if st.highestSeen && p.Seq < st.highest {
		w.stats.Reordered++
		w.faults.Add(fault.Fault{Kind: fault.ReorderedPacket, Camera: src.Camera, Stream: src.Stream,
			Span: fault.Span{StartSeq: p.Seq, EndSeq: p.Seq}, Message: "packet arrived after a higher sequence"})
	}
	if p.Seq != st.next && len(st.pending) >= w.maxPending {
		w.stats.Backpressure++
		return nil, fmt.Errorf("%w: source=%s/%s seq=%d pending=%d", ErrBackpressure, src.Camera, src.Stream, p.Seq, len(st.pending))
	}
	if p.Seq > st.next {
		w.faults.Add(fault.Fault{Kind: fault.Gap, Camera: src.Camera, Stream: src.Stream,
			Span: fault.Span{StartSeq: st.next, EndSeq: p.Seq - 1}, Message: "sequence gap opened by late arrival"})
	}

	st.pending[p.Seq] = p
	if !st.highestSeen || p.Seq > st.highest {
		st.highest = p.Seq
		st.highestSeen = true
	}

	var ready []Packet
	for {
		pkt, ok := st.pending[st.next]
		if !ok {
			break
		}
		delete(st.pending, st.next)
		st.next++
		ready = append(ready, pkt)
		w.stats.Delivered++
	}
	return ready, nil
}

// MissingSpans computes missing inclusive sequence spans up to lastSeq without
// iterating every sequence number.
func (w *Window) MissingSpans(src Source, lastSeq uint64) []fault.Span {
	st := w.sources[src]
	if st == nil {
		if lastSeq == 0 {
			return nil
		}
		return []fault.Span{{StartSeq: 0, EndSeq: lastSeq}}
	}
	seqs := make([]uint64, 0, len(st.pending))
	for seq := range st.pending {
		if seq >= st.next && seq <= lastSeq {
			seqs = append(seqs, seq)
		}
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })

	var spans []fault.Span
	cursor := st.next
	for _, seq := range seqs {
		if seq > lastSeq {
			break
		}
		if seq > cursor {
			spans = append(spans, fault.Span{StartSeq: cursor, EndSeq: seq - 1})
		}
		cursor = seq + 1
	}
	if cursor <= lastSeq {
		spans = append(spans, fault.Span{StartSeq: cursor, EndSeq: lastSeq})
	}
	return fault.MergeSequenceSpans(spans)
}

// RecordLosses converts missing spans to burst-loss faults and updates counters.
func (w *Window) RecordLosses(src Source, lastSeq uint64) []fault.Span {
	spans := w.MissingSpans(src, lastSeq)
	for _, span := range spans {
		n := int(span.EndSeq - span.StartSeq + 1)
		w.stats.BurstLost += n
		w.faults.Add(fault.Fault{Kind: fault.BurstLoss, Camera: src.Camera, Stream: src.Stream, Span: span,
			Message: fmt.Sprintf("burst loss of %d packet(s)", n)})
	}
	return spans
}

// AdvanceRecovered injects reconstructed packets and returns contiguous output.
func (w *Window) AdvanceRecovered(packets []Packet) ([]Packet, error) {
	var out []Packet
	for _, p := range packets {
		ready, err := w.Ingest(p)
		if err != nil {
			return out, err
		}
		out = append(out, ready...)
	}
	return out, nil
}

// NextSeq exposes the contiguous frontier for tests and recovery planning.
func (w *Window) NextSeq(src Source) uint64 {
	if st := w.sources[src]; st != nil {
		return st.next
	}
	return 0
}
