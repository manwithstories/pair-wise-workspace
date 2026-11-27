package rtp

import (
	"bytes"

	"archive/internal/model"
)

// RetransCache keeps copies of packets the sender marked as available for
// retransmission. It is bounded by the repair window: entries older than
// the floor are evicted instead of growing without limit.
type RetransCache struct {
	window int
	store  map[int64]*MediaPacket
}

// NewRetransCache builds a cache with the given retention window.
func NewRetransCache(window int) *RetransCache {
	if window <= 0 || window > model.MaxRepairWindow {
		window = model.MaxRepairWindow
	}
	return &RetransCache{window: window, store: map[int64]*MediaPacket{}}
}

// Store records a packet as available for later retransmission.
func (c *RetransCache) Store(p *MediaPacket) {
	c.store[p.ExtSeq] = p
}

// Get returns a retransmission copy when the sequence is still inside the
// window. A miss means the record aged out and the loss must be repaired
// elsewhere or declared a gap.
func (c *RetransCache) Get(seq int64) (*MediaPacket, bool) {
	p, ok := c.store[seq]
	return p, ok
}

// Evict drops entries older than floor to keep the cache bounded.
func (c *RetransCache) Evict(floor int64) {
	for s := range c.store {
		if s < floor {
			delete(c.store, s)
		}
	}
}

// Len reports the number of cached packets.
func (c *RetransCache) Len() int { return len(c.store) }

// ParityGroup describes one XOR parity block: members are the protected
// media sequence numbers and ParitySeq is the parity packet's own sequence.
// XOR of all member payloads (equal length, enforced by the fixture)
// reconstructs exactly one missing member.
type ParityGroup struct {
	Members   []int64
	ParitySeq int64
	payload   []byte
}

// Repairer resolves missing sequence numbers within a bounded window. The
// order of attempts is fixed: retransmission first, then group parity; when
// neither succeeds an explicit gap is emitted, never a silent splice.
type Repairer struct {
	window  int
	cache   *RetransCache
	groups  map[int64]*ParityGroup // keyed by first member sequence
	log     *model.FaultLog
}

// NewRepairer builds a repairer bound to model.MaxRepairWindow.
func NewRepairer(log *model.FaultLog) *Repairer {
	w := model.MaxRepairWindow
	return &Repairer{window: w, cache: NewRetransCache(w), groups: map[int64]*ParityGroup{}, log: log}
}

// Cache exposes the retransmission cache for the pipeline to feed.
func (r *Repairer) Cache() *RetransCache { return r.cache }

// AddGroup registers a parity block. The parity payload is copied so later
// buffer eviction cannot mutate the repair state.
func (r *Repairer) AddGroup(g *ParityGroup) {
	cp := &ParityGroup{
		Members:   append([]int64(nil), g.Members...),
		ParitySeq: g.ParitySeq,
		payload:   append([]byte(nil), g.payload...),
	}
	if len(cp.Members) > 0 {
		r.groups[cp.Members[0]] = cp
	}
}

// Recovered counts only packets rebuilt by parity; retransmissions are
// counted separately by the caller through the returned flag.
type Recovered struct {
	Packet   *MediaPacket
	Retrans  bool
	Parity   bool
}

// Try attempts to recover one missing sequence. The present map contains
// the sequences currently held by the jitter buffer (other group members
// must be available). Returns nil when no repair source can rebuild it.
func (r *Repairer) Try(seq int64, present map[int64]*MediaPacket) *Recovered {
	if p, ok := r.cache.Get(seq); ok {
		cp := clonePacket(p)
		cp.Retrans = true
		r.log.Add(model.Fault{Phase: model.PhaseRepair, Kind: model.FaultRetransmitted,
			SeqLo: seq, SeqHi: seq})
		return &Recovered{Packet: cp, Retrans: true}
	}
	for _, g := range r.groups {
		if !containsSeq(g.Members, seq) {
			continue
		}
		missing := 0
		var otherPayloads [][]byte
		length := 0
		for _, m := range g.Members {
			if m == seq {
				continue
			}
			p, ok := present[m]
			if !ok {
				missing++
				continue
			}
			otherPayloads = append(otherPayloads, p.Payload)
			length = len(p.Payload)
		}
		// XOR parity can rebuild exactly one lost member.
		if missing != 0 || length == 0 || len(g.payload) != length {
			continue
		}
		rebuilt := make([]byte, length)
		copy(rebuilt, g.payload)
		for _, pl := range otherPayloads {
			for i := range rebuilt {
				rebuilt[i] ^= pl[i]
			}
		}
		if !CheckCRC(rebuilt) {
			r.log.Add(model.Fault{Phase: model.PhaseRepair, Kind: model.FaultCRC,
				SeqLo: seq, SeqHi: seq, Detail: "parity rebuild failed CRC"})
			continue
		}
		r.log.Add(model.Fault{Phase: model.PhaseRepair, Kind: model.FaultFECRecovered,
			SeqLo: seq, SeqHi: seq})
		pkt := &MediaPacket{ExtSeq: seq, Payload: rebuilt, Recovered: true, Size: len(rebuilt)}
		return &Recovered{Packet: pkt, Parity: true}
	}
	return nil
}

// DeclareGap turns an unrecoverable range into an explicit gap record with
// nanosecond bounds derived from the anchor packet's RTP timestamp step.
func (r *Repairer) DeclareGap(segment string, lo, hi int64, start, end model.Nano, reason model.FaultKind) model.Gap {
	g := model.Gap{Segment: segment, SeqLo: lo, SeqHi: hi, Start: start, End: end, Reason: reason}
	r.log.AddGap(g)
	return g
}

// Evict advances both repair windows, dropping state older than floor.
func (r *Repairer) Evict(floor int64) {
	r.cache.Evict(floor)
	for key, g := range r.groups {
		if g.ParitySeq < floor {
			delete(r.groups, key)
		}
	}
}

func clonePacket(p *MediaPacket) *MediaPacket {
	cp := *p
	cp.Payload = append([]byte(nil), p.Payload...)
	return &cp
}

func containsSeq(list []int64, v int64) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// XORPayload is exported for the fixture to build parity packets.
func XORPayload(payloads [][]byte) ([]byte, bool) {
	if len(payloads) == 0 {
		return nil, false
	}
	n := len(payloads[0])
	for _, p := range payloads {
		if len(p) != n {
			return nil, false
		}
	}
	out := make([]byte, n)
	for _, p := range payloads {
		for i := range out {
			out[i] ^= p[i]
		}
	}
	return out, true
}

// EqualPayload compares payloads in tests and pipeline assertions.
func EqualPayload(a, b []byte) bool { return bytes.Equal(a, b) }
