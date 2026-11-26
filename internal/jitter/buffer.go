package jitter

import (
	"fmt"
	"sort"

	"mediaqc/internal/media"
	rtpio "mediaqc/internal/rtp"
)

// Default limits keep reorder state bounded on a long-lived terminal process.
const (
	DefaultWaitMS     = 60
	DefaultMaxPackets = 256
)

// Gap is an explicit, non-synthesized break in the RTP timeline.
type Gap struct {
	SSRC       uint32   `json:"ssrc"`
	FromExtSeq uint64   `json:"from_ext_seq"`
	ToExtSeq   uint64   `json:"to_ext_seq"`
	Missing    []uint64 `json:"missing"`
	DetectedMS int64    `json:"detected_ms"`
	Reason     string   `json:"reason"`
}

// Len returns the number of missing sequence numbers in the gap.
func (g Gap) Len() int { return len(g.Missing) }

// Stats contains jitter buffer counters.
type Stats struct {
	Buffered  int `json:"buffered"`
	Delivered int `json:"delivered"`
	Gaps      int `json:"gaps"`
	Late      int `json:"late"`
	Reordered int `json:"reordered"`
}

// Buffer is a bounded adaptive reordering window for one RTP source.
type Buffer struct {
	ssrc       uint32
	next       uint64
	waitMS     int
	maxPackets int
	items      map[uint64]*rtpio.Packet
	arrival    map[uint64]int64
	maxPushed  uint64
	lastNowMS  int64
	started    bool
	Stats      Stats
}

// NewBuffer creates a buffer beginning at expectedExtSeq.
func NewBuffer(ssrc uint32, expectedExtSeq uint64, waitMS, maxPackets int) *Buffer {
	if waitMS <= 0 {
		waitMS = DefaultWaitMS
	}
	if maxPackets <= 0 {
		maxPackets = DefaultMaxPackets
	}
	return &Buffer{
		ssrc: ssrc, next: expectedExtSeq, waitMS: waitMS, maxPackets: maxPackets,
		items: make(map[uint64]*rtpio.Packet), arrival: make(map[uint64]int64),
	}
}

// SetWaitMS adapts the playout window from the bandwidth trajectory.
func (b *Buffer) SetWaitMS(waitMS int) {
	if waitMS > 0 {
		b.waitMS = waitMS
	}
}

// Next returns the next expected extended sequence number.
func (b *Buffer) Next() uint64 { return b.next }

// Push buffers one accepted RTP packet.
func (b *Buffer) Push(p *rtpio.Packet) error {
	if p == nil {
		return media.New(media.KindMalformed, "jitter.Push", "nil packet")
	}
	if p.SSRC != b.ssrc {
		return media.New(media.KindInternal, "jitter.Push",
			fmt.Sprintf("ssrc mismatch: want=%08x got=%08x", b.ssrc, p.SSRC))
	}
	if p.ExtSequence < b.next {
		b.Stats.Late++
		return nil
	}
	if _, exists := b.items[p.ExtSequence]; exists {
		// Ingest normally removes exact duplicates; this keeps the buffer safe.
		return nil
	}
	if len(b.items) >= b.maxPackets {
		return media.New(media.KindInternal, "jitter.Push", "bounded reorder queue full")
	}
	if p.ExtSequence > b.next {
		b.Stats.Reordered++
	}
	b.started = true
	b.items[p.ExtSequence] = p
	b.arrival[p.ExtSequence] = p.ArrivalMS
	if p.ExtSequence > b.maxPushed {
		b.maxPushed = p.ExtSequence
	}
	b.Stats.Buffered++
	return nil
}

// Advance releases contiguous packets and classifies holes whose adaptive
// waiting window has expired.
func (b *Buffer) Advance(nowMS int64) ([]*rtpio.Packet, []Gap) {
	b.lastNowMS = nowMS
	var out []*rtpio.Packet
	var gaps []Gap

	for {
		if p, ok := b.items[b.next]; ok {
			delete(b.items, b.next)
			delete(b.arrival, b.next)
			out = append(out, p)
			b.Stats.Delivered++
			b.next++
			continue
		}

		future, ok := b.minFuture()
		if !ok {
			return out, gaps
		}
		seenAt := b.arrival[future]
		if nowMS < seenAt+int64(b.waitMS) {
			return out, gaps
		}

		missing := make([]uint64, 0, future-b.next)
		for seq := b.next; seq < future; seq++ {
			missing = append(missing, seq)
		}
		gap := Gap{
			SSRC: b.ssrc, FromExtSeq: b.next, ToExtSeq: future - 1,
			Missing: missing, DetectedMS: nowMS, Reason: "adaptive_window_expired",
		}
		gaps = append(gaps, gap)
		b.Stats.Gaps += len(missing)
		b.next = future
	}
}

// FlushThrough releases the stream through expectedLast, marking tail loss.
func (b *Buffer) FlushThrough(expectedLast uint64) ([]*rtpio.Packet, []Gap) {
	flushNow := b.lastNowMS + int64(b.waitMS)
	out, gaps := b.Advance(flushNow)
	if b.next <= expectedLast {
		missing := make([]uint64, 0, expectedLast-b.next+1)
		for seq := b.next; seq <= expectedLast; seq++ {
			if _, buffered := b.items[seq]; buffered {
				continue
			}
			missing = append(missing, seq)
		}
		if len(missing) > 0 {
			gap := Gap{
				SSRC: b.ssrc, FromExtSeq: missing[0], ToExtSeq: missing[len(missing)-1],
				Missing: missing, DetectedMS: flushNow, Reason: "end_of_stream",
			}
			gaps = append(gaps, gap)
			b.Stats.Gaps += len(missing)
		}
		for seq := b.next; seq <= expectedLast; seq++ {
			if p, ok := b.items[seq]; ok {
				out = append(out, p)
				b.Stats.Delivered++
				delete(b.items, seq)
			}
		}
		b.next = expectedLast + 1
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExtSequence < out[j].ExtSequence })
	return out, gaps
}

func (b *Buffer) minFuture() (uint64, bool) {
	min := uint64(0)
	ok := false
	for seq := range b.items {
		if seq < b.next {
			continue
		}
		if !ok || seq < min {
			min, ok = seq, true
		}
	}
	return min, ok
}
