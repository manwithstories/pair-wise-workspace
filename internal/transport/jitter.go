package transport

import (
	"sync"

	"avlab/internal/media"
)

// gapState tracks one missing sequence window.
type gapState struct {
	seq       uint16
	expireAt  uint64
	retried   bool
	segment   uint32
	keyframe  bool
}

// JitterStats is the full recovery scorecard.
type JitterStats struct {
	Played          uint64
	BurstRecovered  uint64
	BurstUnrecovered uint64
	DeadlineSkips   uint64
	KeyframeJumps   uint64
	GapsOpen        uint64
	GapsClosed      uint64
	QueuePeak       int
}

// JitterBuffer is a bounded playout buffer with simulated clock. Packets that
// miss their playout deadline are skipped to the next keyframe; retransmissions
// arriving before the deadline close the gap transparently.
type JitterBuffer struct {
	mu sync.Mutex

	capacity int
	waitTick uint64
	now      uint64
	next     uint16
	haveNext bool

	queued  map[uint16]*Packet
	gaps    map[uint16]*gapState
	stats   JitterStats
}

// NewJitterBuffer creates a buffer. capacity is hard-capped by
// media.MaxJitterPackets; waitTick is the maximum retransmission wait window.
func NewJitterBuffer(capacity int, waitTick uint64) *JitterBuffer {
	if capacity <= 0 || capacity > media.MaxJitterPackets {
		capacity = media.MaxJitterPackets
	}
	return &JitterBuffer{
		capacity: capacity,
		waitTick: waitTick,
		queued:   map[uint16]*Packet{},
		gaps:     map[uint16]*gapState{},
	}
}

// Push admits a packet that passed the reorder stage.
func (j *JitterBuffer) Push(p *Packet) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.haveNext {
		j.next = p.Seq()
		j.haveNext = true
	}
	if seqLt(p.Seq(), j.next) {
		return media.NewFault(media.CodePacketLate, true,
			"packet older than playout cursor").WithSeq(p.Seq(), p.Epoch)
	}
	if len(j.queued) >= j.capacity {
		return media.NewFault(media.CodeLimitExceeded, true,
			"jitter buffer capacity reached").WithSeq(p.Seq(), p.Epoch)
	}
	j.queued[p.Seq()] = p
	if g, ok := j.gaps[p.Seq()]; ok {
		// retransmission arrived in time
		delete(j.gaps, p.Seq())
		j.stats.GapsClosed++
		if j.now <= g.expireAt {
			j.stats.BurstRecovered++
		}
	}
	if n := len(j.queued); n > j.stats.QueuePeak {
		j.stats.QueuePeak = n
	}
	return nil
}

// MarkLost opens a gap for a packet the trace dropped. The first retry is
// scheduled at the trace-driven retransmission tick.
func (j *JitterBuffer) MarkLost(seq uint16, segment uint32, keyframe bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.haveNext {
		j.next = seq
		j.haveNext = true
	}
	if _, ok := j.gaps[seq]; ok || !seqLt(j.next, seq) && seq != j.next {
		return
	}
	j.gaps[seq] = &gapState{
		seq: seq, expireAt: j.now + j.waitTick,
		segment: segment, keyframe: keyframe,
	}
	j.stats.GapsOpen++
}

// Playout advances the simulated clock and returns every packet that can be
// played, in sequence order. Expired gaps produce a keyframe jump.
func (j *JitterBuffer) Playout(now uint64) []*Packet {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.now = now
	var out []*Packet
	for {
		if p, ok := j.queued[j.next]; ok {
			out = append(out, p)
			delete(j.queued, j.next)
			j.stats.Played++
			j.next++
			continue
		}
		g, missing := j.gaps[j.next]
		if !missing {
			break
		}
		if now < g.expireAt {
			break // still inside the retransmission window
		}
		// deadline expired: skip past the gap toward the next keyframe
		delete(j.gaps, j.next)
		j.stats.DeadlineSkips++
		j.stats.BurstUnrecovered++
		j.skipToNextKeyframe()
		// loop again so the landing keyframe is played immediately
	}
	return out
}

// skipToNextKeyframe advances the cursor past queued non-keyframe packets
// until the next keyframe. The cursor stops ON the keyframe so the outer
// playout loop emits it immediately. If no keyframe is buffered, the cursor
// moves to the end of the queued range so playout resumes on the next push.
func (j *JitterBuffer) skipToNextKeyframe() uint16 {
	j.next++
	for {
		p, ok := j.queued[j.next]
		if !ok {
			break
		}
		if p.Keyframe {
			break // land on, do not consume, the keyframe
		}
		delete(j.queued, j.next)
		j.next++
	}
	j.stats.KeyframeJumps++
	return j.next
}

// Stats returns a snapshot of the recovery counters.
func (j *JitterBuffer) Stats() JitterStats {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.stats
}

// Queued returns the current occupancy.
func (j *JitterBuffer) Queued() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.queued)
}
