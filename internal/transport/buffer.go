package transport

import (
	"sort"

	"avcheck/internal/media"
)

// Packet is one received RTP payload with its arrival time on the shared
// virtual clock.
type Packet struct {
	Stream    uint16
	Seq       uint32
	Timestamp uint32
	Marker    bool
	Payload   []byte
	ArrivalNs int64
}

// Stats are monotonic per-buffer counters.
type Stats struct {
	Accepted   int
	Duplicates int
	Reordered  int
	Late       int
	TooFar     int
	Emitted    int
	GapPackets int
	GapRuns    int
	PeakBytes  int64
	TotalBytes int64
}

type entry struct {
	pkt     Packet
	readyNs int64
}

// Buffer is a single-stream bounded jitter buffer. It is not safe for
// concurrent use; the pipeline owns one per track.
type Buffer struct {
	cfg     Config
	faults  *media.Faults
	stream  uint16
	started bool
	next    uint32 // next sequence number to emit
	high    uint32 // highest sequence number seen
	pending map[uint32]*entry
	readyAt []uint32
	bytes   int64
	stats   Stats
	closed  bool
	// gap tracking: sequence numbers known missing behind the high-water mark.
	missing map[uint32]struct{}
}

// NewBuffer creates a buffer for one stream.
func NewBuffer(stream uint16, cfg Config, faults *media.Faults) (*Buffer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if faults == nil {
		faults = &media.Faults{}
	}
	return &Buffer{
		cfg:     cfg,
		faults:  faults,
		stream:  stream,
		pending: make(map[uint32]*entry),
		missing: make(map[uint32]struct{}),
	}, nil
}

// Stats returns a copy of the counters.
func (b *Buffer) Stats() Stats { return b.stats }

// Close releases the queue immediately; subsequent pushes fail with
// ErrCanceled. The pipeline calls it on cancellation so no buffer outlives
// the run.
func (b *Buffer) Close() {
	b.closed = true
	b.pending = nil
	b.readyAt = nil
	b.missing = nil
	b.bytes = 0
}

// Bytes is the currently queued payload size.
func (b *Buffer) Bytes() int64 { return b.bytes }

// Len is the number of queued packets.
func (b *Buffer) Len() int { return len(b.pending) }

// OldestDeadline returns the earliest release deadline among queued packets,
// or 0 when the queue is empty.
func (b *Buffer) OldestDeadline() int64 {
	first := true
	var d int64
	for _, e := range b.pending {
		if first || e.readyNs < d {
			d = e.readyNs
			first = false
		}
	}
	return d
}

// Push accepts a packet. Duplicates are counted and ignored; packets behind
// the emit cursor are late; packets too far ahead of the cursor are rejected
// as a reorder-window fault. The byte cap triggers ErrBackpressure.
func (b *Buffer) Push(p Packet) error {
	if b.closed {
		return ErrCanceled
	}
	p.Payload = append([]byte(nil), p.Payload...)
	size := int64(len(p.Payload))
	if b.bytes+size > b.cfg.MaxBufferBytes {
		b.faults.Addf(media.LevelDegraded, media.CodeSlowConsumer, uint64(p.Stream), int64(p.Seq),
			"queue would hold %d bytes, cap %d", b.bytes+size, b.cfg.MaxBufferBytes)
		return ErrBackpressure
	}
	if !b.started {
		b.started = true
		b.next = p.Seq
		b.high = p.Seq
	}
	if _, dup := b.pending[p.Seq]; dup {
		b.stats.Duplicates++
		b.faults.Addf(media.LevelRecovered, media.CodeDuplicatePacket, uint64(p.Stream), int64(p.Seq),
			"duplicate sequence %d ignored", p.Seq)
		return nil
	}
	if seqLess(p.Seq, b.next) {
		// Before anything has been emitted the cursor is only a guess made
		// from the first arrival; a jittered packet that overtook its
		// neighbours must still be accepted, not treated as late.
		if b.stats.Emitted == 0 && SeqDiff(b.next, p.Seq) < int32(b.cfg.ReorderWindow) {
			b.next = p.Seq
		} else {
			b.stats.Late++
			return nil
		}
	}
	dist := int32(p.Seq - b.next)
	if dist < 0 {
		// Sequence wrapped behind the cursor: treat as late, not as a huge
		// forward jump.
		b.stats.Late++
		return nil
	}
	if dist > int32(b.cfg.ReorderWindow) {
		b.stats.TooFar++
		b.faults.Addf(media.LevelDegraded, media.CodeJitterOverflow, uint64(p.Stream), int64(p.Seq),
			"sequence %d is %d ahead of cursor", p.Seq, dist)
		return nil
	}
	if seqLess(b.high, p.Seq) {
		b.high = p.Seq
	}
	if seqLess(b.next, p.Seq) && len(b.pending) > 0 {
		// Arrived after a higher sequence was already queued.
		b.stats.Reordered++
	}
	b.pending[p.Seq] = &entry{pkt: p, readyNs: p.ArrivalNs + b.cfg.LatencyNs}
	b.bytes += size
	b.stats.Accepted++
	b.stats.TotalBytes += size
	if b.bytes > b.stats.PeakBytes {
		b.stats.PeakBytes = b.bytes
	}
	return nil
}

// seen reports whether seq is at or before the emit cursor.
func (b *Buffer) seen(seq uint32) bool { return !seqLess(b.next, seq) }

// PopReady emits every buffered packet that is contiguous from the cursor or
// whose jitter deadline has passed by nowNs, in sequence order. Missing
// sequence numbers between emitted packets are recorded as explicit gaps.
func (b *Buffer) PopReady(nowNs int64) []Packet {
	if b.closed {
		return nil
	}
	var out []Packet
	for len(b.pending) > 0 {
		e, ok := b.pending[b.next]
		if !ok {
			// Cursor is missing. Either the next queued packet has waited
			// past its deadline (emit and record a gap) or we wait.
			nxt, deadline := b.nextPresent()
			if nxt == nil || deadline > nowNs {
				break
			}
			b.recordGap(b.next, *nxt)
			b.next = *nxt
			continue
		}
		if e.readyNs > nowNs {
			break
		}
		out = append(out, b.emit(b.next))
	}
	return out
}

// nextPresent returns the lowest buffered sequence at or after the cursor
// and its earliest release deadline.
func (b *Buffer) nextPresent() (*uint32, int64) {
	var best *uint32
	var deadline int64
	for s, e := range b.pending {
		if seqLess(s, b.next) {
			continue
		}
		if best == nil || seqLess(s, *best) {
			cp := s
			best = &cp
			deadline = e.readyNs
		} else if s == *best && e.readyNs < deadline {
			deadline = e.readyNs
		}
	}
	if best == nil {
		return nil, 0
	}
	return best, deadline
}

// Flush emits all remaining buffered packets in sequence order, recording
// every gap between them. Trailing loss beyond the highest received packet is
// not invented; the caller reports it from its own expectation of group size.
func (b *Buffer) Flush() []Packet {
	if b.closed {
		return nil
	}
	seqs := make([]uint32, 0, len(b.pending))
	for s := range b.pending {
		if !seqLess(s, b.next) {
			seqs = append(seqs, s)
		}
	}
	sort.Slice(seqs, func(i, j int) bool {
		return int32(seqs[i]-b.next) < int32(seqs[j]-b.next)
	})
	var out []Packet
	for _, s := range seqs {
		if s != b.next {
			b.recordGap(b.next, s)
			b.next = s
		}
		out = append(out, b.emit(s))
	}
	// Anything in pending below the cursor is stale; drop it.
	b.pending = make(map[uint32]*entry)
	b.bytes = 0
	b.readyAt = b.readyAt[:0]
	return out
}

// emit removes and returns one packet at seq.
func (b *Buffer) emit(seq uint32) Packet {
	e := b.pending[seq]
	delete(b.pending, seq)
	b.bytes -= int64(len(e.pkt.Payload))
	b.stats.Emitted++
	b.next = seq + 1
	return e.pkt
}

// recordGap logs missing sequence numbers in [from,to).
func (b *Buffer) recordGap(from, to uint32) {
	gap := int32(to - from)
	if gap <= 0 {
		return
	}
	b.stats.GapRuns++
	b.stats.GapPackets += int(gap)
	b.faults.Addf(media.LevelDegraded, media.CodeJitterOverflow, uint64(b.stream), int64(from),
		"%d packet gap [%d,%d)", gap, from, to)
	for s := from; s != to; s++ {
		b.missing[s] = struct{}{}
	}
}

// Missing returns the sorted set of sequence numbers recorded as gaps.
func (b *Buffer) Missing() []uint32 {
	out := make([]uint32, 0, len(b.missing))
	for s := range b.missing {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		return int32(out[i]-b.next) < int32(out[j]-b.next)
	})
	return out
}
