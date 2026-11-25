// Package transport recovers shard payloads from duplicated, reordered and
// partially missing RTP packets.
package transport

import (
	"context"
	"fmt"

	"github.com/pion/rtp"

	"fieldpost/internal/core"
)

// DefaultWindow is the bounded reorder window in sequence positions.
const DefaultWindow uint16 = 64

// MaxRetries is the exact local-source retry budget for one missing sequence.
const MaxRetries = 2

// FetchFunc reloads one packet from the local, already-generated source.
type FetchFunc func(ctx context.Context, seq uint16) (*rtp.Packet, error)

// AddResult classifies an incoming packet without changing retry accounting.
type AddResult int

const (
	Added AddResult = iota
	Duplicate
	OutsideWindow
)

// Report records recovery observables used by the demo and tests.
type Report struct {
	Received   int
	Duplicates int
	Reordered  int
	Retries    int
	Recovered  int
	Missing    []uint16
	Base       uint16
	Count      int
}

// ReorderBuffer deduplicates and orders packets inside a bounded seq window.
type ReorderBuffer struct {
	next    uint16
	base    uint16
	window  uint16
	packets map[uint16]*rtp.Packet
	seen    map[uint16]struct{}
	report  Report
}

// NewReorderBuffer creates a buffer whose first expected sequence is base.
func NewReorderBuffer(base, window uint16) *ReorderBuffer {
	if window == 0 {
		window = DefaultWindow
	}
	return &ReorderBuffer{
		next: base, base: base, window: window,
		packets: make(map[uint16]*rtp.Packet),
		seen:    make(map[uint16]struct{}),
		report:  Report{Base: base},
	}
}

// SeqDelta returns to-from as a signed forward distance modulo 65536.
func SeqDelta(from, to uint16) int {
	d := int(int32(to) - int32(from))
	if d > 32767 {
		d -= 65536
	}
	if d < -32768 {
		d += 65536
	}
	return d
}

// Add inserts an RTP packet if it is new and inside the bounded window.
func (b *ReorderBuffer) Add(pkt *rtp.Packet) AddResult {
	if pkt == nil {
		return OutsideWindow
	}
	seq := pkt.SequenceNumber
	b.report.Received++
	if _, ok := b.seen[seq]; ok {
		b.report.Duplicates++
		return Duplicate
	}
	delta := SeqDelta(b.next, seq)
	if delta < 0 {
		// Already delivered or an old duplicate after map compaction.
		b.seen[seq] = struct{}{}
		b.report.Duplicates++
		return Duplicate
	}
	if delta >= int(b.window) {
		return OutsideWindow
	}
	if delta > 0 {
		b.report.Reordered++
	}
	b.seen[seq] = struct{}{}
	b.packets[seq] = pkt
	return Added
}

// ParseAndAdd unmarshals a wire packet and inserts it into the buffer.
func (b *ReorderBuffer) ParseAndAdd(raw []byte) (AddResult, error) {
	pkt := &rtp.Packet{}
	if err := pkt.Unmarshal(raw); err != nil {
		return OutsideWindow, core.New(core.StageTransport, core.ExitUnrecoverable, false,
			"invalid RTP packet: %v", err)
	}
	return b.Add(pkt), nil
}

// Drain returns all currently contiguous payloads in sequence order.
func (b *ReorderBuffer) Drain() [][]byte {
	var out [][]byte
	for {
		pkt, ok := b.packets[b.next]
		if !ok {
			return out
		}
		out = append(out, append([]byte(nil), pkt.Payload...))
		delete(b.packets, b.next)
		b.next++
	}
}

// Buffered reports how many positions are retained for reordering.
func (b *ReorderBuffer) Buffered() int { return len(b.packets) }

// Next returns the next contiguous expected sequence.
func (b *ReorderBuffer) Next() uint16 { return b.next }

// Report returns a copy of recovery counters.
func (b *ReorderBuffer) Report() Report { return b.report }

// RecoverSegment orders count packets beginning at base. Missing packets are
// retried from local source at most twice each. A remaining gap produces a
// non-retryable, locatable core.Error so the caller can isolate the segment.
func RecoverSegment(
	ctx context.Context,
	segmentID string,
	base uint16,
	count int,
	incoming []*rtp.Packet,
	fetch FetchFunc,
	window uint16,
) ([][]byte, Report, error) {
	if count <= 0 {
		return nil, Report{Base: base}, core.UnrecoverableError(
			core.StageTransport, "rtp", segmentID, int64(base), "empty segment")
	}
	b := NewReorderBuffer(base, window)
	b.report.Count = count
	for _, pkt := range incoming {
		b.Add(pkt)
	}
	ordered := make([][]byte, 0, count)
	for i := 0; i < count; i++ {
		if err := ctx.Err(); err != nil {
			return nil, b.report, core.Wrap(core.StageTransport, 1, false, err, "recovery canceled")
		}
		seq := base + uint16(i)
		pkt, ok := b.packets[seq]
		if !ok {
			var lastErr error
			for attempt := 1; attempt <= MaxRetries; attempt++ {
				b.report.Retries++
				if fetch == nil {
					lastErr = fmt.Errorf("no local source")
					break
				}
				p, err := fetch(ctx, seq)
				if err != nil {
					lastErr = err
					continue
				}
				if p != nil {
					res := b.Add(p)
					if res != Duplicate {
						b.report.Recovered++
					}
					lastErr = nil
					break
				}
			}
			pkt, ok = b.packets[seq]
			if !ok {
				b.report.Missing = append(b.report.Missing, seq)
				return nil, b.report, core.UnrecoverableError(
					core.StageTransport, "local-source", segmentID, int64(seq),
					"RTP sequence %d missing after %d local retries: %v",
					seq, MaxRetries, lastErr)
			}
		}
		ordered = append(ordered, append([]byte(nil), pkt.Payload...))
		delete(b.packets, seq)
		b.next = seq + 1
	}
	return ordered, b.report, nil
}
