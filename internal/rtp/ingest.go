package rtp

import (
	"mediamix/internal/clock"
)

// ReorderConfig tunes the jitter reorder buffer.
type ReorderConfig struct {
	// MaxReorder is the largest out-of-order distance the buffer waits for.
	// Packets further ahead than this flush the buffer immediately.
	MaxReorder int
	// MaxWait is the maximum number of subsequent arrivals tolerated before
	// a missing sequence number is declared permanently lost.
	MaxWait int
}

// DefaultReorderConfig is used when no explicit tuning is supplied.
func DefaultReorderConfig() ReorderConfig {
	return ReorderConfig{MaxReorder: 64, MaxWait: 32}
}

// Stats summarises ingest behaviour for metrics and recovery accounting.
type Stats struct {
	Received   int
	Duplicates int
	Reordered  int
	Lost       int // gap entries that timed out or were flushed
	Wrapped    int // observed 16-bit sequence wraps
	MaxJump    int // largest forward jump observed
}

// Reorderer accepts packets in arrival order and emits them in sequence
// order. It detects duplicates, reordering, losses and sequence-number
// wraps using modular arithmetic.
type Reorderer struct {
	cfg      ReorderConfig
	started  bool
	expected uint16
	lastSeq  uint16 // sequence number of the previous arriving packet
	haveLast bool
	buf      map[uint16]*Packet
	age      map[uint16]int
	stats    Stats
	lastLost uint16
}

// NewReorderer creates a reorderer.
func NewReorderer(cfg ReorderConfig) *Reorderer {
	if cfg.MaxReorder <= 0 {
		cfg.MaxReorder = 64
	}
	if cfg.MaxWait <= 0 {
		cfg.MaxWait = 32
	}
	return &Reorderer{cfg: cfg, buf: make(map[uint16]*Packet), age: make(map[uint16]int)}
}

// Push inserts a packet. It returns the packets that can now be released in
// sequence order (zero or more), plus a non-fatal MediaError describing a
// recoverable condition when one occurred (duplicate, gap, wrap).
func (r *Reorderer) Push(p *Packet) ([]*Packet, *clock.MediaError) {
	r.stats.Received++
	if !r.started {
		r.started = true
		r.expected = p.Seq
	}
	jump := clock.Wrap16(r.expected, p.Seq)
	if jump > r.stats.MaxJump {
		r.stats.MaxJump = jump
	}
	wrapErr := r.detectWrap(p.Seq)
	r.lastSeq, r.haveLast = p.Seq, true

	if _, exists := r.buf[p.Seq]; exists {
		return nil, r.duplicate(p.Seq)
	}
	if jump < 0 {
		// Older than the next expected seq and not queued: the gap was
		// already released or declared lost, so this arrival is unusable.
		return nil, r.duplicate(p.Seq)
	}
	var gapErr *clock.MediaError
	if jump > r.cfg.MaxReorder {
		// Wild jump: flush everything before it as lost rather than buffering.
		gapErr = clock.NewError(clock.StageIngest, clock.ErrGap, p.Seq, true,
			"forward jump exceeds reorder window; flushing gap")
		out := r.flushBefore(p.Seq)
		r.expected = p.Seq
		out = append(out, r.bufferOrRelease(p)...)
		return out, gapErr
	}
	// jump > 1 means the packet arrived before its predecessors: reordered.
	if jump > 1 {
		r.stats.Reordered++
	}
	out := r.bufferOrRelease(p)
	r.ageSeqs()
	return out, wrapErr
}

// bufferOrRelease stores the packet and then releases every consecutive
// sequence number available starting at expected.
func (r *Reorderer) bufferOrRelease(p *Packet) []*Packet {
	r.buf[p.Seq] = p
	r.age[p.Seq] = 0
	var out []*Packet
	for {
		next, ok := r.buf[r.expected]
		if !ok {
			r.declareAgedLosses()
			return out
		}
		out = append(out, next)
		delete(r.buf, r.expected)
		delete(r.age, r.expected)
		r.expected++
	}
}

// ageSeqs grows the age of every still-buffered sequence; anything older
// than MaxWait without its predecessor arriving is declared lost.
func (r *Reorderer) ageSeqs() {
	for s := range r.age {
		r.age[s]++
	}
	r.declareAgedLosses()
}

func (r *Reorderer) declareAgedLosses() {
	for {
		if _, ok := r.buf[r.expected]; ok {
			return
		}
		var oldest uint16
		found := false
		for s := range r.buf {
			if !found || clock.Wrap16(r.expected, s) < clock.Wrap16(r.expected, oldest) {
				oldest, found = s, true
			}
		}
		if !found {
			return
		}
		if r.age[oldest] < r.cfg.MaxWait {
			return
		}
		// Everything strictly before the oldest buffered packet is lost.
		r.markLostUntil(oldest)
		r.expected = oldest
	}
}

// flushBefore marks every seq up to (but excluding) target as lost and
// returns any queued packets in sequence order.
func (r *Reorderer) flushBefore(target uint16) []*Packet {
	var out []*Packet
	for clock.Wrap16(target, r.expected) > 0 {
		if p, ok := r.buf[r.expected]; ok {
			out = append(out, p)
			delete(r.buf, r.expected)
			delete(r.age, r.expected)
		} else {
			r.markLost(r.expected)
		}
		r.expected++
	}
	return out
}

func (r *Reorderer) markLostUntil(until uint16) {
	for r.expected != until {
		r.markLost(r.expected)
		r.expected++
	}
}

func (r *Reorderer) markLost(seq uint16) {
	r.stats.Lost++
	r.lastLost = seq
}

func (r *Reorderer) duplicate(seq uint16) *clock.MediaError {
	r.stats.Duplicates++
	return clock.NewError(clock.StageIngest, clock.ErrDuplicate, seq, true, "duplicate packet discarded")
}

func (r *Reorderer) detectWrap(seq uint16) *clock.MediaError {
	// Observed when an arriving packet lands in the low end of the 16-bit
	// space while the previous arrival was in the high end.
	if r.haveLast && seq < 0x100 && r.lastSeq > 0xFF00 {
		r.stats.Wrapped++
		return clock.NewError(clock.StageIngest, clock.ErrWrap, seq, true, "sequence number wrap observed")
	}
	return nil
}

// Flush releases all remaining buffered packets in order, marking the
// remaining gaps as lost. Call it at stream end.
func (r *Reorderer) Flush() []*Packet {
	var out []*Packet
	for len(r.buf) > 0 {
		if p, ok := r.buf[r.expected]; ok {
			out = append(out, p)
			delete(r.buf, r.expected)
			delete(r.age, r.expected)
		} else {
			r.markLost(r.expected)
		}
		r.expected++
	}
	return out
}

// Stats returns a copy of the running ingest statistics.
func (r *Reorderer) Stats() Stats { return r.stats }
