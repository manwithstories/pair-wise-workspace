package transport

import (
	"sort"

	"mediad/internal/model"
)

// Engine runs one ordered, recovery-enabled pass over an impaired stream.
type Engine struct {
	jb   *JitterBuffer
	nack *nackCache
	cfg  JitterConfig

	// original packets indexed by sequence, used to build parity and to
	// satisfy NACK for packets the injector merely reordered.
	originals map[uint16]*Packet
	parity    map[uint16][]byte // block base -> parity payload

	recovered map[uint16]bool // idempotency guard across resume
	mechanism map[uint16]string
	delivered map[uint16]*Packet
	gaps      []Gap
	stats     RecoveryStats
}

// NewEngine wires a jitter buffer, NACK cache and FEC state.
func NewEngine(cfg JitterConfig) *Engine {
	return &Engine{
		jb:        NewJitterBuffer(cfg),
		nack:      newNACKCache(cfg.NACKDepth),
		cfg:       cfg,
		originals: make(map[uint16]*Packet),
		parity:    make(map[uint16][]byte),
		recovered: make(map[uint16]bool),
		mechanism: make(map[uint16]string),
		delivered: make(map[uint16]*Packet),
	}
}

// Ingest registers the clean, pre-impairment originals so FEC parity can be
// computed and NACK retransmission has a source of truth.
func (e *Engine) Ingest(pkts []*Packet) {
	for _, p := range pkts {
		seq := p.RTP.SequenceNumber
		e.originals[seq] = p
		blk := blockFor(seq)
		if seq == blk.seqA {
			// parity is computed once both members are known; defer via pair
			if b, ok := e.originals[blk.seqB]; ok {
				e.parity[blk.seqA] = xorPayload(p.RTP.Payload, b.RTP.Payload)
			}
		} else if existing, ok := e.originals[blk.seqA]; ok {
			e.parity[blk.seqA] = xorPayload(existing.RTP.Payload, p.RTP.Payload)
		}
	}
	e.stats.TotalPackets = len(pkts)
}

// Receive accepts one packet observed on the impaired path.
func (e *Engine) Receive(p *Packet) {
	if e.jb.Push(p) {
		e.nack.add(p)
	}
}

// tryRecover attempts NACK then XOR for a single missing sequence. Returns
// true when a packet was reinstated into the jitter buffer.
func (e *Engine) tryRecover(seq uint16) bool {
	if e.recovered[seq] {
		return true
	}
	// 1) NACK: the packet exists but was reordered/delayed in the cache.
	if p := e.nack.lookup(seq); p != nil {
		e.jb.Push(p)
		e.recovered[seq] = true
		e.mechanism[seq] = "nack"
		e.stats.NACKRecovered++
		return true
	}
	// 2) single-packet XOR using the block parity.
	blk := blockFor(seq)
	parity, ok := e.parity[blk.seqA]
	if !ok {
		return false
	}
	var survivorSeq uint16
	if seq == blk.seqA {
		survivorSeq = blk.seqB
	} else {
		survivorSeq = blk.seqA
	}
	// XOR needs exactly one surviving member. The survivor must have been
	// observed on the wire; if both members were dropped, recovery is
	// impossible and the hole stays an unrecoverable timeline gap.
	if !e.jb.Has(survivorSeq) {
		return false
	}
	survivor, ok := e.originals[survivorSeq]
	if !ok {
		return false
	}
	rec := reconstruct(seq, survivor, parity)
	e.jb.Push(rec)
	e.recovered[seq] = true
	e.mechanism[seq] = "xor"
	e.stats.XORRecovered++
	return true
}

// Mechanism reports how a recovered sequence was repaired ("nack" or "xor"),
// or an empty string when no recovery was needed for it.
func (e *Engine) Mechanism(seq uint16) string { return e.mechanism[seq] }

// Finalize flushes the buffer once the full impaired stream has been
// observed. It walks every sequence number from the first up to the
// high-water mark exactly once: present packets drain directly, missing
// ones go through NACK then XOR recovery, and holes that cannot be repaired
// become timeline gaps. Bounded iteration rules out any livelock.
func (e *Engine) Finalize() ([]*Packet, []Gap, RecoveryStats, map[uint16]string) {
	var maxSeq uint16
	hasMax := false
	for seq := range e.originals {
		if !hasMax || seqDist(maxSeq, seq) > 0 {
			maxSeq, hasMax = seq, true
		}
	}
	if !hasMax {
		return nil, nil, e.stats, e.mechanism
	}
	first := e.originalFirstSeq()
	// unsigned forward span inclusive of both ends
	total := int(maxSeq-first) + 1

	for step := 0; step < total; step++ {
		seq := first + uint16(step)
		if p, ok := e.jb.present[seq]; ok {
			e.delivered[seq] = p
			delete(e.jb.present, seq)
			continue
		}
		// missing: attempt recovery (NACK first, then single-packet XOR)
		if e.tryRecover(seq) {
			if p, ok := e.jb.present[seq]; ok {
				e.delivered[seq] = p
				delete(e.jb.present, seq)
				continue
			}
		}
		e.recordGap(seq)
	}
	e.stats.RecoveredPackets = e.stats.NACKRecovered + e.stats.XORRecovered
	e.stats.DeliveredPackets = len(e.delivered)
	e.stats.LostPackets = e.stats.TotalPackets - e.stats.DeliveredPackets
	if e.stats.LostPackets < 0 {
		e.stats.LostPackets = 0
	}
	e.stats.UnrecoveredPackets = e.stats.LostPackets
	ordered := make([]*Packet, 0, len(e.delivered))
	for _, p := range e.delivered {
		ordered = append(ordered, p)
	}
	sort.Slice(ordered, func(i, k int) bool {
		return seqDist(ordered[i].RTP.SequenceNumber, ordered[k].RTP.SequenceNumber) > 0
	})
	return ordered, e.gaps, e.stats, e.mechanism
}

// originalFirstSeq returns the smallest original sequence number.
func (e *Engine) originalFirstSeq() uint16 {
	var first uint16
	set := false
	for seq := range e.originals {
		if !set || seqDist(first, seq) < 0 {
			first, set = seq, true
		}
	}
	return first
}

func (e *Engine) recordGap(seq uint16) {
	pts := int64(0)
	if orig, ok := e.originals[seq]; ok {
		pts = int64(orig.PTS)
	}
	if n := len(e.gaps); n > 0 && e.gaps[n-1].EndSeq+1 == seq {
		e.gaps[n-1].EndSeq = seq
		e.gaps[n-1].Count++
		return
	}
	e.gaps = append(e.gaps, Gap{StartSeq: seq, EndSeq: seq, PTS: pts, Count: 1})
}

// Run is the one-call convenience wrapper over the full recovery path. It
// returns delivered packets, aged gaps, stats and the per-seq mechanism map.
func Run(originals, impaired []*Packet, cfg JitterConfig) (
	[]*Packet, []Gap, RecoveryStats, map[uint16]string) {
	eng := NewEngine(cfg)
	eng.Ingest(originals)
	for _, p := range impaired {
		eng.Receive(p)
	}
	return eng.Finalize()
}

// GapsToFaults converts transport gaps into the shared fault model so the
// error propagates into sync and provenance records.
func GapsToFaults(gaps []Gap, source string) []*model.Fault {
	out := make([]*model.Fault, 0, len(gaps))
	for _, g := range gaps {
		out = append(out, &model.Fault{
			Kind:   model.FaultOutOfBounds,
			Offset: model.OffsetUnknown,
			Source: source,
			Detail: "timeline gap: missing RTP seq " + u16s(g.StartSeq) +
				".." + u16s(g.EndSeq),
		})
	}
	return out
}

func u16s(v uint16) string {
	const digits = "0123456789"
	if v == 0 {
		return "0"
	}
	var b [5]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = digits[v%10]
		v /= 10
	}
	return string(b[i:])
}
