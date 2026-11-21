// Package transport turns lossy, duplicated, reordered synthetic RTP streams
// into per-representation ordered frame tracks. Memory is bounded by the
// reorder window, never by the total clip length.
package transport

import (
	"fmt"

	"github.com/pion/rtp"

	"mediasync/internal/model"
)

// ReorderConfig tunes one representation's receive buffer.
type ReorderConfig struct {
	Window int // packets held before a gap is declared unrecoverable
}

// DefaultReorderConfig is sized for typical bursts in the 12s fixtures.
func DefaultReorderConfig() ReorderConfig { return ReorderConfig{Window: 32} }

type timedPacket struct {
	pkt                  *rtp.Packet
	extTS                int64 // unwrapped RTP timestamp in track-clock ticks
	late                 bool  // a higher sequence had already arrived when this one came
	sendTicks, recvTicks int64
}

// ReorderBuffer performs per-representation deduplication, reordering and
// burst-loss recovery. It is a single-goroutine state machine.
type ReorderBuffer struct {
	cfg        ReorderConfig
	rep        string
	track      string
	clock      model.Clock
	started    bool
	nextSeq    int64 // next expected extended sequence number
	maxSeq     int64 // highest extended sequence number ever observed
	lastTS     int64 // last unwrapped RTP timestamp
	tsStarted  bool
	pending    map[int64]timedPacket
	diag       model.Bag
	duplicates int
	recovered  int
	gaps       int
	emitted    map[int64]bool
}

// NewReorderBuffer builds a buffer for one representation.
func NewReorderBuffer(track, rep string, clock model.Clock, cfg ReorderConfig) *ReorderBuffer {
	return &ReorderBuffer{
		cfg:     cfg,
		rep:     rep,
		track:   track,
		clock:   clock,
		pending: map[int64]timedPacket{},
		emitted: map[int64]bool{},
	}
}

// Push accepts one received packet (possibly duplicated or out of order) and
// returns every frame that can now be released in sequence order. Packets that
// arrive after their gap was declared are returned as recovered frames; gap
// placeholders carry Incomplete=true and never fabricate payload.
func (b *ReorderBuffer) Push(pkt *rtp.Packet, sendTicks, recvTicks int64) []model.Frame {
	ext := b.extendSeq(pkt.SequenceNumber)
	if b.emitted[ext] || b.hasPending(ext) {
		b.duplicates++
		b.diag.Add(model.Diagnostic{Kind: model.KindDuplicatePacket, Severity: model.SeverityWarning,
			Track: b.track, AtTicks: int64(pkt.Timestamp), Recoverable: true,
			Message: fmt.Sprintf("rep=%s duplicate seq=%d suppressed", b.rep, ext)})
		return nil
	}
	if b.started && ext < b.nextSeq {
		// Gap already declared/flushed: late data cannot resurrect a placeholder.
		b.duplicates++
		return nil
	}
	late := b.started && ext < b.maxSeq
	b.pending[ext] = timedPacket{pkt: pkt, extTS: b.extendTS(pkt.Timestamp), late: late,
		sendTicks: sendTicks, recvTicks: recvTicks}
	if !b.started || ext > b.maxSeq {
		b.maxSeq = ext
	}
	return b.drain()
}

func (b *ReorderBuffer) hasPending(seq int64) bool { _, ok := b.pending[seq]; return ok }

// Flush declares the stream ended: any remaining gaps become unrecoverable
// placeholders and buffered packets are released.
func (b *ReorderBuffer) Flush() []model.Frame {
	if !b.started {
		return nil
	}
	var out []model.Frame
	for b.nextSeq <= b.maxSeq {
		out = append(out, b.releaseNext())
	}
	b.pending = map[int64]timedPacket{}
	return out
}

func (b *ReorderBuffer) drain() []model.Frame {
	var out []model.Frame
	// A gap is unrecoverable once the highest received seq is at least Window
	// ahead of it: no in-window reordering can fill it anymore.
	for b.maxSeq-b.nextSeq >= int64(b.cfg.Window) {
		out = append(out, b.releaseNext())
	}
	// Release contiguous head packets; a packet filling a still-open hole is
	// recovered traffic, the initial contiguous run is not.
	for b.hasPending(b.nextSeq) {
		out = append(out, b.releaseNext())
	}
	return out
}

// releaseNext emits exactly one sequence position: a buffered packet or, when
// none exists, an explicit incomplete placeholder (never fabricated content).
func (b *ReorderBuffer) releaseNext() model.Frame {
	seq := b.nextSeq
	b.nextSeq++
	tp, ok := b.pending[seq]
	if !ok {
		return b.emitGap(seq)
	}
	delete(b.pending, seq)
	b.emitted[seq] = true
	recovered := tp.late
	if recovered {
		b.recovered++
	}
	return model.Frame{
		Track: b.track, Rep: b.rep, Kind: model.KindVideo, Clock: b.clock,
		Seq: seq, Timestamp: tp.extTS, Marker: tp.pkt.Marker,
		Payload: append([]byte(nil), tp.pkt.Payload...), Recovered: recovered,
		SendTicks: tp.sendTicks, RecvTicks: tp.recvTicks,
	}
}

func (b *ReorderBuffer) emitGap(seq int64) model.Frame {
	b.gaps++
	b.diag.Add(model.Diagnostic{Kind: model.KindPacketGap, Severity: model.SeverityError,
		Track: b.track, AtTicks: model.NoTimestamp,
		Message: fmt.Sprintf("rep=%s unrecoverable gap at seq=%d (window=%d)", b.rep, seq, b.cfg.Window)})
	return model.Frame{Track: b.track, Rep: b.rep, Kind: model.KindVideo, Clock: b.clock,
		Seq: seq, Incomplete: true}
}

func (b *ReorderBuffer) extendSeq(seq uint16) int64 {
	if !b.started {
		b.started = true
		b.nextSeq = int64(seq)
		b.maxSeq = int64(seq)
		return int64(seq)
	}
	return model.Unwrap16(b.maxSeq, seq)
}

// extendTS unwraps the 32-bit RTP timestamp against the previous extended value.
func (b *ReorderBuffer) extendTS(ts uint32) int64 {
	if !b.tsStarted {
		b.tsStarted = true
		b.lastTS = int64(ts)
		return int64(ts)
	}
	ext := model.Unwrap32(b.lastTS, ts)
	b.lastTS = ext
	return ext
}

// Stats exposes recovery counters for metrics and idempotency checks.
type Stats struct {
	Rep        string
	Recovered  int
	Gaps       int
	Duplicates int
	Emitted    int
}

// Stats returns a point-in-time counter snapshot.
func (b *ReorderBuffer) Stats() Stats {
	return Stats{Rep: b.rep, Recovered: b.recovered, Gaps: b.gaps,
		Duplicates: b.duplicates, Emitted: len(b.emitted)}
}

// Diagnostics returns the diagnostics collected so far.
func (b *ReorderBuffer) Diagnostics() []model.Diagnostic { return b.diag.All() }

// Pending returns how many packets are buffered; used by memory-bound tests.
func (b *ReorderBuffer) Pending() int { return len(b.pending) }
