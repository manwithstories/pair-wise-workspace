// Package transport ingests RTP packets, removes duplicates, restores sequence
// order across 16-bit wraparound and tracks gaps for later recovery.
package transport

import (
	"mediamix/internal/core"
	"mediamix/internal/timeline"
)

// ReorderBuffer reorders one track's packets and reports the blocking gap
// instead of blocking indefinitely.
//
// Invariant: drain() always leaves expected pointing at a sequence that is not
// buffered. Therefore there is at most one open gap at a time: the run from
// expected up to the lowest buffered sequence. That makes recovery exact — a
// gap is either fully repaired or abandoned, never partially consumed.
type ReorderBuffer struct {
	track    string
	limits   Limits
	ext      *Extender
	expected uint64
	started  bool
	buf      map[uint64]Packet
	digests  map[uint64]uint64
	maxSeen  uint64
	seenAny  bool
	gapSince timeline.Micros
	haveGap  bool
	stats    Stats
	closed   bool
}

// NewReorderBuffer creates the per-track buffer.
func NewReorderBuffer(track string, limits Limits) (*ReorderBuffer, error) {
	if track == "" {
		return nil, &core.Error{Code: core.CodeBadArgument, Detail: "empty track id", Fatal: true}
	}
	if limits.MaxBuffered <= 0 {
		limits = DefaultLimits()
	}
	return &ReorderBuffer{
		track:   track,
		limits:  limits,
		ext:     NewExtender(),
		buf:     make(map[uint64]Packet),
		digests: make(map[uint64]uint64),
	}, nil
}

// Rebind changes the buffer bound after a bandwidth re-sample. If the new
// bound is below what is currently queued, the buffer advances past the hole
// immediately so an unbound-latency queue is trimmed at the moment capacity
// drops, not after it overflows.
func (r *ReorderBuffer) Rebind(maxBuffered int) ([]Packet, int) {
	if maxBuffered <= 0 {
		return nil, 0
	}
	shrank := maxBuffered < r.limits.MaxBuffered
	r.limits.MaxBuffered = maxBuffered
	if !shrank || len(r.buf) <= maxBuffered {
		return nil, 0
	}
	out, skipped := r.Advance()
	if skipped > 0 {
		r.stats.Timeouts++
		r.stats.Skipped += skipped
	}
	return out, skipped
}

// Track returns the buffer's track id.
func (r *ReorderBuffer) Track() string { return r.track }

// Stats returns a snapshot of counters.
func (r *ReorderBuffer) Stats() Stats { return r.stats }

// Expected returns the next extended sequence number to be delivered.
func (r *ReorderBuffer) Expected() uint64 { return r.expected }

// Buffered returns the number of packets held back by the gap.
func (r *ReorderBuffer) Buffered() int { return len(r.buf) }

// OpenGap returns the current blocking gap, if any.
func (r *ReorderBuffer) OpenGap() (Gap, bool) {
	if len(r.buf) == 0 {
		return Gap{}, false
	}
	next := r.lowestBuffered()
	if next <= r.expected {
		return Gap{}, false
	}
	return Gap{
		Track:    r.track,
		StartExt: r.expected,
		EndExt:   next - 1,
		Count:    int(next - r.expected),
		SinceUs:  r.gapSince,
	}, true
}

// lowestBuffered returns the smallest buffered sequence strictly after expected.
func (r *ReorderBuffer) lowestBuffered() uint64 {
	first := uint64(0)
	found := false
	for ext := range r.buf {
		if ext >= r.expected && (!found || ext < first) {
			first = ext
			found = true
		}
	}
	if !found {
		return r.expected
	}
	return first
}

// Push accepts a packet and returns, in order, every packet that became
// deliverable. A duplicate never re-enters the buffer; a conflicting duplicate
// keeps the first copy and is counted separately.
func (r *ReorderBuffer) Push(p Packet, now timeline.Micros) ([]Packet, error) {
	if r.closed {
		return nil, &core.Error{Code: core.CodeBadPacket, Track: r.track, Detail: "buffer closed", Fatal: true}
	}
	if p.Track == "" {
		p.Track = r.track
	}
	if p.Track != r.track {
		return nil, core.SeqError(core.CodeBadPacket, r.track, uint32(p.Seq), "packet routed to wrong track buffer")
	}
	if p.ArrivalUs < 0 || len(p.Payload) == 0 {
		return nil, core.SeqError(core.CodeBadPacket, r.track, uint32(p.Seq), "empty payload or negative arrival")
	}
	ext, err := r.ext.Extend(r.track, p.Seq)
	if err != nil {
		return nil, core.StampTrack(err, r.track)
	}
	p.Ext = ext
	if !r.started {
		r.started = true
		r.expected = ext
	}
	r.stats.Received++
	if d, seen := r.digests[ext]; seen {
		if d != p.digest() {
			r.stats.Conflicts++
			return nil, core.SeqError(core.CodeBadPacket, r.track, uint32(p.Seq),
				"duplicate sequence with conflicting payload")
		}
		r.stats.Duplicates++
		return nil, nil
	}
	if ext < r.expected {
		// The cursor already passed this sequence: late arrival.
		r.stats.Late++
		r.stats.Reordered++
		r.digests[ext] = p.digest()
		return []Packet{p}, nil
	}
	if !r.seenAny || ext > r.maxSeen {
		r.maxSeen = ext
		r.seenAny = true
	} else {
		// Arrived after a higher sequence: the stream was out of order.
		r.stats.Reordered++
	}
	r.buf[ext] = p
	r.digests[ext] = p.digest()
	r.pruneDigests()
	if len(r.buf) > r.limits.MaxBuffered {
		// The queue outgrew its latency budget: advance past the hole instead
		// of letting delay grow without bound.
		out, skipped := r.Advance()
		if skipped > 0 {
			r.stats.Timeouts++
			r.stats.Skipped += skipped
		}
		r.stats.PeakBuffered = maxInt(r.stats.PeakBuffered, len(r.buf))
		return out, nil
	}
	r.stats.PeakBuffered = maxInt(r.stats.PeakBuffered, len(r.buf))
	out := r.drain()
	r.noteGap(now)
	return out, nil
}

// Insert places a retransmitted packet and drains whatever is now contiguous.
// It is the recovery path and is validated by the caller's recovery source.
func (r *ReorderBuffer) Insert(p Packet) []Packet {
	if p.Ext < r.expected {
		return nil
	}
	if _, dup := r.buf[p.Ext]; dup {
		return nil
	}
	r.buf[p.Ext] = p
	r.digests[p.Ext] = p.digest()
	if p.Ext > r.maxSeen {
		r.maxSeen = p.Ext
	}
	return r.drain()
}

// pruneDigests bounds duplicate-detection memory: once a packet is far behind
// the cursor, keeping its digest costs memory for little value, since a very
// late duplicate is treated as late rather than reordered.
func (r *ReorderBuffer) pruneDigests() {
	limit := r.limits.MaxBuffered * 4
	if len(r.digests) <= limit {
		return
	}
	cutoff := uint64(0)
	if r.expected > uint64(limit) {
		cutoff = r.expected - uint64(limit)
	}
	for ext := range r.digests {
		if ext < cutoff {
			delete(r.digests, ext)
		}
	}
}

// drain emits the contiguous run starting at expected.
func (r *ReorderBuffer) drain() []Packet {
	var out []Packet
	for {
		p, ok := r.buf[r.expected]
		if !ok {
			break
		}
		delete(r.buf, r.expected)
		out = append(out, p)
		r.expected++
	}
	r.stats.Delivered += len(out)
	if len(r.buf) == 0 {
		r.haveGap = false
	}
	return out
}

// noteGap stamps the moment the current gap first appeared.
func (r *ReorderBuffer) noteGap(now timeline.Micros) {
	_, open := r.OpenGap()
	if !open {
		r.haveGap = false
		return
	}
	if !r.haveGap {
		r.haveGap = true
		r.gapSince = now
	}
}

// GapAgeMs returns how long the current gap has been waiting.
func (r *ReorderBuffer) GapAgeMs(now timeline.Micros) float64 {
	if _, ok := r.OpenGap(); !ok {
		return 0
	}
	return float64(now-r.gapSince) / 1000
}
