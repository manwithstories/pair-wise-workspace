// Package jitter manages a bounded per-track reorder buffer keyed by
// extended sequence number and event time.
package jitter

import (
	"container/heap"
	"sort"

	"mediad/internal/ingest"
	"mediad/internal/media"
)

// Default bounds: at most 2 seconds of media or 64 MiB per track.
const (
	DefaultMaxDuration = 2 * media.NanosPerSecond
	DefaultMaxBytes    = int64(64) << 20
)

// Drop records the first packet a queue had to discard.
type Drop struct {
	Track  uint32
	ExtSeq uint64
	At     int64
	Reason string
}

type seqHeap []uint64

func (h seqHeap) Len() int           { return len(h) }
func (h seqHeap) Less(i, j int) bool { return h[i] < h[j] }
func (h seqHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *seqHeap) Push(x any)        { *h = append(*h, x.(uint64)) }
func (h *seqHeap) Pop() any {
	old := *h
	n := len(old)
	v := old[n-1]
	*h = old[:n-1]
	return v
}

// Queue is a bounded reorder buffer for one track. It never grows past
// its configured duration or byte limit; the first discarded packet and
// the reason are recorded instead of growing without bound.
type Queue struct {
	track       uint32
	maxDuration int64
	maxBytes    int64

	items map[uint64]*ingest.Packet
	times map[uint64]media.MediaTime
	order seqHeap
	bytes int64
	minT  media.MediaTime
	maxT  media.MediaTime

	// FirstDrop is the first packet discarded because of the bounds.
	FirstDrop *Drop
	Drops     int
}

func New(track uint32, maxDuration, maxBytes int64) *Queue {
	if maxDuration <= 0 {
		maxDuration = DefaultMaxDuration
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	return &Queue{
		track:       track,
		maxDuration: maxDuration,
		maxBytes:    maxBytes,
		items:       make(map[uint64]*ingest.Packet),
		times:       make(map[uint64]media.MediaTime),
	}
}

func (q *Queue) Len() int { return len(q.order) }

func (q *Queue) drop(p *ingest.Packet, reason string) *media.Fault {
	q.Drops++
	if q.FirstDrop == nil {
		q.FirstDrop = &Drop{Track: q.track, ExtSeq: p.ExtSeq, At: p.At, Reason: reason}
	}
	return &media.Fault{
		Kind: media.FaultBufferOverflow, Op: "jitter.queue",
		Track: q.track, Seq: p.ExtSeq, At: p.At,
		Msg: "queue bound exceeded: " + reason,
	}
}

// Fits reports whether a packet with the given event time and payload
// size could be buffered without exceeding the bounds, and if not, why.
func (q *Queue) Fits(t media.MediaTime, payloadBytes int) (bool, string) {
	if q.bytes+int64(payloadBytes) > q.maxBytes {
		return false, "byte-limit"
	}
	if len(q.order) > 0 {
		lo, hi := q.minT, q.maxT
		if media.Compare(t, lo) < 0 {
			lo = t
		}
		if media.Compare(t, hi) > 0 {
			hi = t
		}
		if hi.Sub(lo) > q.maxDuration {
			return false, "duration-limit"
		}
	}
	return true, ""
}

// Push buffers p at its event time. A packet already buffered is
// ignored (duplicates are classified upstream by recovery). When a
// bound would be exceeded the incoming packet is dropped and a
// buffer-overflow fault is returned.
func (q *Queue) Push(p *ingest.Packet, t media.MediaTime) *media.Fault {
	if _, ok := q.items[p.ExtSeq]; ok {
		return nil
	}
	if ok, reason := q.Fits(t, len(p.Payload)); !ok {
		return q.drop(p, reason)
	}
	if len(q.order) == 0 {
		q.minT, q.maxT = t, t
	} else {
		if media.Compare(t, q.minT) < 0 {
			q.minT = t
		}
		if media.Compare(t, q.maxT) > 0 {
			q.maxT = t
		}
	}
	q.items[p.ExtSeq] = p
	q.times[p.ExtSeq] = t
	heap.Push(&q.order, p.ExtSeq)
	q.bytes += int64(len(p.Payload))
	return nil
}

// Entry is one buffered packet with its event time.
type Entry struct {
	Packet *ingest.Packet
	Time   media.MediaTime
}

// Snapshot returns the buffered entries in extended-sequence order,
// e.g. for checkpointing.
func (q *Queue) Snapshot() []Entry {
	seqs := append([]uint64(nil), q.order...)
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	out := make([]Entry, 0, len(seqs))
	for _, s := range seqs {
		out = append(out, Entry{Packet: q.items[s], Time: q.times[s]})
	}
	return out
}

// Head returns the smallest buffered extended sequence and its time.
func (q *Queue) Head() (uint64, media.MediaTime, bool) {
	if len(q.order) == 0 {
		return 0, media.MediaTime{}, false
	}
	s := q.order[0]
	return s, q.times[s], true
}

// Pop removes and returns the packet with the smallest extended
// sequence number.
func (q *Queue) Pop() (*ingest.Packet, media.MediaTime, bool) {
	if len(q.order) == 0 {
		return nil, media.MediaTime{}, false
	}
	s := heap.Pop(&q.order).(uint64)
	p := q.items[s]
	t := q.times[s]
	delete(q.items, s)
	delete(q.times, s)
	q.bytes -= int64(len(p.Payload))
	if len(q.order) > 0 && (media.Compare(t, q.minT) == 0 || media.Compare(t, q.maxT) == 0) {
		q.recomputeSpan()
	}
	return p, t, true
}

func (q *Queue) recomputeSpan() {
	first := true
	for _, t := range q.times {
		if first || media.Compare(t, q.minT) < 0 {
			q.minT = t
		}
		if first || media.Compare(t, q.maxT) > 0 {
			q.maxT = t
		}
		first = false
	}
}
