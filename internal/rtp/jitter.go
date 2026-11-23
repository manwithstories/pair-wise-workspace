package rtp

import (
	"github.com/pion/rtp"

	"github.com/example/mediad/internal/clock"
	"github.com/example/mediad/internal/model"
)

// Config tunes the jitter buffer. All windows are expressed in packets.
type Config struct {
	ClockRate  uint32 // RTP clock rate, e.g. 90000 for video
	WindowSize uint16 // reorder / late-packet recovery window
	MaxPackets int    // hard capacity bounding per-stream memory
	SegmentOf  func(seq uint16) string
}

// Unit is an ordered media element on the unified timeline.
type Unit struct {
	Seq         uint16
	Timestamp   uint32
	Time        clock.Micros
	Marker      bool
	PayloadType uint8
	SSRC        uint32
	Payload     []byte
}

// Stats summarises one buffer run.
type Stats struct {
	Received             int
	Duplicates           int
	Reordered            int
	Recovered            int // gaps closed by a late packet inside the window
	UnrecoverablePackets int // gaps that expired without the packet
	QuarantinedSegments  []string
}

// Buffer reorders RTP packets, removes duplicates, drives the finite
// recovery policy and emits ordered Units on the unified timeline.
type Buffer struct {
	cfg    Config
	clocks *clock.RTPClock
	rec    *recoverer
	store  map[uint16]*rtp.Packet
	base   uint16 // next sequence number due to be emitted
	start  bool
	maxSeq uint16 // highest sequence number observed so far
	out    []*Unit
	quar   map[string]struct{}
	stats  Stats
}

// NewBuffer validates the configuration and creates an empty buffer.
func NewBuffer(cfg Config) (*Buffer, error) {
	if cfg.WindowSize == 0 {
		cfg.WindowSize = 32
	}
	if cfg.MaxPackets == 0 {
		cfg.MaxPackets = 4096
	}
	if cfg.ClockRate == 0 {
		return nil, model.New(model.KindArgument, "rtp", "jitter",
			"clock rate is required")
	}
	return &Buffer{
		cfg:    cfg,
		clocks: clock.NewRTPClock(cfg.ClockRate, 0),
		rec:    newRecoverer(cfg.WindowSize),
		store:  map[uint16]*rtp.Packet{},
		quar:   map[string]struct{}{},
	}, nil
}

// forwardDist returns b-a interpreted as a signed 16-bit distance.
func forwardDist(a, b uint16) int { return int(int16(b - a)) }

// Push accepts one raw RTP packet. Duplicates are counted and dropped,
// out-of-order packets are queued, and gaps beyond the recovery window
// become unrecoverable and quarantine their segment.
func (b *Buffer) Push(pkt *rtp.Packet) error {
	if pkt == nil {
		return model.New(model.KindArgument, "rtp", "jitter", "nil packet")
	}
	seq := pkt.SequenceNumber
	b.stats.Received++

	if _, ok := b.store[seq]; ok {
		b.stats.Duplicates++
		return nil
	}
	if !b.start {
		b.start = true
		b.base = seq
		b.maxSeq = seq
	} else if forwardDist(b.base, seq) < 0 {
		// Already-emitted sequence number arriving again.
		b.stats.Duplicates++
		return nil
	}

	switch d := forwardDist(b.maxSeq, seq); {
	case d > 0:
		// Forward jump: register every sequence number skipped.
		b.rec.registerGap(b.maxSeq, seq)
		b.maxSeq = seq
	case d < 0:
		// Late packet. It only helps while the gap is still open.
		if b.rec.noteLate(seq) {
			b.stats.Recovered++
			b.stats.Reordered++
		} else {
			// Too late: gap already expired, nothing can use this.
			b.stats.Duplicates++
			return nil
		}
	default:
		// d == 0 on a seq not in store can only happen at startup.
	}

	if len(b.store) >= b.cfg.MaxPackets {
		return model.New(model.KindTransient, "rtp", "jitter",
			"jitter buffer capacity exceeded")
	}
	b.store[seq] = pkt
	b.drain()
	return nil
}

// drain emits every contiguous packet available at the base. It stops
// when the base packet has not arrived yet (a gap or an in-flight
// reorder), unless the gap's recovery window has elapsed.
func (b *Buffer) drain() {
	for {
		if pkt, ok := b.store[b.base]; ok {
			b.emit(pkt)
			delete(b.store, b.base)
			b.base++
			continue
		}
		g, open := b.rec.gaps[b.base]
		if !open {
			// Base packet is simply in flight; wait for it.
			return
		}
		if forwardDist(g.deadline, b.maxSeq) < 0 {
			// Window still open: a late packet may still repair it.
			return
		}
		// Window elapsed: declare unrecoverable and skip the seq.
		for _, s := range b.rec.expireAt(b.base) {
			b.stats.UnrecoverablePackets++
			b.quarantine(s)
		}
		b.base++
	}
}

func (b *Buffer) emit(pkt *rtp.Packet) {
	b.out = append(b.out, &Unit{
		Seq:         pkt.SequenceNumber,
		Timestamp:   pkt.Timestamp,
		Time:        b.clocks.Convert(pkt.Timestamp),
		Marker:      pkt.Marker,
		PayloadType: pkt.PayloadType,
		SSRC:        pkt.SSRC,
		Payload:     append([]byte(nil), pkt.Payload...),
	})
}

func (b *Buffer) quarantine(seq uint16) {
	if b.cfg.SegmentOf == nil {
		return
	}
	seg := b.cfg.SegmentOf(seq)
	if seg == "" {
		return
	}
	if _, ok := b.quar[seg]; !ok {
		b.quar[seg] = struct{}{}
		b.stats.QuarantinedSegments = append(b.stats.QuarantinedSegments, seg)
	}
}

// Pop returns the next ordered unit, or ok=false when nothing is ready.
func (b *Buffer) Pop() (*Unit, bool) {
	if len(b.out) == 0 {
		return nil, false
	}
	u := b.out[0]
	b.out = b.out[1:]
	return u, true
}

// Flush forces every still-open gap to expire and returns the remaining
// ordered units. It is called at end-of-stream.
func (b *Buffer) Flush() []*Unit {
	// One past the highest sequence number the buffer may ever emit.
	limit := b.maxSeq + 1
	for b.base != limit {
		if _, ok := b.store[b.base]; ok {
			b.drain()
			continue
		}
		// No packet will ever arrive for base: expire and move on.
		for _, s := range b.rec.expireAt(b.base) {
			b.stats.UnrecoverablePackets++
			b.quarantine(s)
		}
		b.base++
	}
	b.drain()
	rest := b.out
	b.out = nil
	return rest
}

// Stats returns a snapshot of buffer counters.
func (b *Buffer) Stats() Stats { return b.stats }
