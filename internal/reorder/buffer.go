// Package reorder implements a bounded reorder buffer. Packets are keyed by
// their 64-bit extended sequence number and delivered in order. The buffer is
// bounded both by a sequence window (packets older than it can no longer be
// accepted) and by a byte budget; late, duplicate-conflicting and
// over-budget packets are counted and reported instead of silently dropped.
package reorder

import (
	"mediapipeline/internal/mediaerr"
	"mediapipeline/internal/stream"
)

// Gap records a span of extended sequence numbers that will never be filled
// in time: either the window passed or the stream ended before they arrived.
type Gap struct {
	Track uint32
	From  uint64
	To    uint64 // inclusive
}

// Stats exposes reorder-buffer counters for the metrics report.
type Stats struct {
	Accepted  int64
	Delivered int64
	Late      int64 // arrived after the window moved past their sequence
	Isolated  int64 // duplicate sequence with a conflicting payload
	Gaps      int64 // number of missing sequence numbers declared
	OverLimit int64 // rejected because the byte budget was exhausted
	PeakBytes int64
}

// Config configures the bounded buffer.
type Config struct {
	// WindowSize bounds the span between the expected and the newest
	// sequence number; a packet arriving at or beyond expected+WindowSize
	// forces the missing tail to be declared as gaps.
	WindowSize int
	// MaxBytes bounds the summed raw datagram size held in the buffer.
	MaxBytes int
}

// Buffer is a bounded, in-order reorder buffer for one track.
type Buffer struct {
	cfg      Config
	track    uint32
	pkts     map[uint64]*stream.Packet
	expected uint64 // next sequence number eligible for delivery
	high     uint64 // highest sequence ever accepted
	started  bool
	bytes    int64
	stats    Stats
}

// New creates a buffer with the given bounds.
func New(track uint32, cfg Config) (*Buffer, *mediaerr.Error) {
	if cfg.WindowSize <= 0 {
		return nil, mediaerr.New(mediaerr.StageReorder, mediaerr.KindInvalidInput,
			"reorder window must be positive")
	}
	if cfg.MaxBytes <= 0 {
		return nil, mediaerr.New(mediaerr.StageReorder, mediaerr.KindInvalidInput,
			"reorder byte budget must be positive")
	}
	return &Buffer{cfg: cfg, track: track, pkts: make(map[uint64]*stream.Packet)}, nil
}

// Push inserts a packet. The returned gaps are sequence spans declared when
// the packet jumps beyond the reorder window. The returned error is non-nil
// for late or conflicting packets; in both cases the packet is counted but
// not stored, and callers may keep processing the stream. A buffer-limit
// error means the packet was rejected against the byte budget.
func (b *Buffer) Push(p *stream.Packet) ([]Gap, *mediaerr.Error) {
	if b.started && p.ExtSeq < b.expected {
		b.stats.Late++
		return nil, mediaerr.New(mediaerr.StageReorder, mediaerr.KindLate,
			"packet arrived after the reorder window moved on").
			WithTrack(b.track).WithSeq(p.Sequence).WithTS(p.Timestamp)
	}
	if _, exists := b.pkts[p.ExtSeq]; exists {
		b.stats.Isolated++
		return nil, mediaerr.New(mediaerr.StageReorder, mediaerr.KindDuplicate,
			"duplicate sequence isolated by reorder buffer").
			WithTrack(b.track).WithSeq(p.Sequence).WithTS(p.Timestamp)
	}
	var gaps []Gap
	if b.started && p.ExtSeq-b.expected >= uint64(b.cfg.WindowSize) {
		gaps = b.skipTo(p.ExtSeq - uint64(b.cfg.WindowSize) + 1)
	}
	size := int64(len(p.Raw))
	if b.bytes+size > int64(b.cfg.MaxBytes) {
		b.stats.OverLimit++
		return gaps, mediaerr.New(mediaerr.StageReorder, mediaerr.KindBufferLimit,
			"reorder buffer byte budget exhausted").
			WithTrack(b.track).WithSeq(p.Sequence).WithTS(p.Timestamp)
	}
	if !b.started {
		b.started = true
		b.expected = p.ExtSeq
		b.high = p.ExtSeq
	}
	b.pkts[p.ExtSeq] = p
	b.bytes += size
	b.stats.Accepted++
	if p.ExtSeq > b.high {
		b.high = p.ExtSeq
	}
	if b.bytes > b.stats.PeakBytes {
		b.stats.PeakBytes = b.bytes
	}
	return gaps, nil
}

// skipTo advances the delivery frontier, declaring gaps for every sequence
// number in between that is not present in the buffer.
func (b *Buffer) skipTo(newExpected uint64) []Gap {
	var gaps []Gap
	var gapStart uint64
	inGap := false
	for s := b.expected; s < newExpected; s++ {
		if old, ok := b.pkts[s]; ok {
			if inGap {
				gaps = append(gaps, Gap{Track: b.track, From: gapStart, To: s - 1})
				inGap = false
			}
			b.bytes -= int64(len(old.Raw))
			delete(b.pkts, s) // would never be popped in time now
		} else if !inGap {
			gapStart = s
			inGap = true
		}
	}
	if inGap {
		gaps = append(gaps, Gap{Track: b.track, From: gapStart, To: newExpected - 1})
	}
	for _, g := range gaps {
		b.stats.Gaps += int64(g.To - g.From + 1)
	}
	b.expected = newExpected
	return gaps
}

// Pop removes and returns the next in-order packet, or nil when the expected
// sequence number has not arrived yet.
func (b *Buffer) Pop() *stream.Packet {
	if !b.started {
		return nil
	}
	p, ok := b.pkts[b.expected]
	if !ok {
		return nil
	}
	delete(b.pkts, b.expected)
	b.bytes -= int64(len(p.Raw))
	b.expected++
	b.stats.Delivered++
	return p
}

// Drain pops every currently contiguous in-order packet.
func (b *Buffer) Drain() []*stream.Packet {
	var out []*stream.Packet
	for {
		p := b.Pop()
		if p == nil {
			return out
		}
		out = append(out, p)
	}
}

// Flush delivers every remaining packet in order and declares gaps for the
// missing sequence numbers up to the highest accepted value.
func (b *Buffer) Flush() (packets []*stream.Packet, gaps []Gap) {
	if !b.started {
		return nil, nil
	}
	packets = b.Drain()
	if b.high >= b.expected {
		var gapStart uint64
		inGap := false
		for s := b.expected; s <= b.high; s++ {
			if old, ok := b.pkts[s]; ok {
				if inGap {
					gaps = append(gaps, Gap{Track: b.track, From: gapStart, To: s - 1})
					inGap = false
				}
				packets = append(packets, old)
				delete(b.pkts, s)
				b.bytes = 0
			} else if !inGap {
				gapStart = s
				inGap = true
			}
		}
		if inGap {
			gaps = append(gaps, Gap{Track: b.track, From: gapStart, To: b.high})
		}
	}
	for _, g := range gaps {
		b.stats.Gaps += int64(g.To - g.From + 1)
	}
	b.expected = b.high + 1
	return packets, gaps
}

// Len returns the number of packets currently held.
func (b *Buffer) Len() int { return len(b.pkts) }

// Bytes returns the summed raw datagram bytes currently held.
func (b *Buffer) Bytes() int64 { return b.bytes }

// Expected returns the next deliverable extended sequence number.
func (b *Buffer) Expected() (uint64, bool) { return b.expected, b.started }

// Stats returns a snapshot of the counters.
func (b *Buffer) Stats() Stats { return b.stats }
