package transport

import (
	"fmt"
	"sort"
	"sync"

	"mediad/internal/model"
)

type missingState struct {
	retries int
	burst   bool
}

type Assembler struct {
	mu       sync.Mutex
	cfg      Config
	nextSeq  uint64
	nextOff  int64
	highest  uint64
	haveHigh bool
	out      []byte
	buffered map[uint64]Packet
	seen     map[Fingerprint]struct{}
	bySeq    map[uint64]Fingerprint
	missing  map[uint64]*missingState
	bufBytes int64
	stats    Stats
}

func NewAssembler(cfg Config) *Assembler {
	cfg.normalize()
	a := &Assembler{
		cfg:      cfg,
		buffered: make(map[uint64]Packet),
		seen:     make(map[Fingerprint]struct{}),
		bySeq:    make(map[uint64]Fingerprint),
		missing:  make(map[uint64]*missingState),
	}
	a.SetBandwidth(cfg.BandwidthBps)
	return a
}

func (a *Assembler) Stats() Stats {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.stats
	s.BufferedPackets = len(a.buffered)
	s.BufferedBytes = a.bufBytes
	return s
}

// RetransmitWindow converts the bandwidth-delay product to bounded packets.
func (a *Assembler) RetransmitWindow() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.windowLocked()
}

func (a *Assembler) SetBandwidth(bps int64) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if bps > 0 {
		a.cfg.BandwidthBps = bps
	}
	a.stats.BandwidthBps = a.cfg.BandwidthBps
	a.stats.WindowPackets = a.windowLocked()
	return a.stats.WindowPackets
}

func (a *Assembler) windowLocked() int {
	pkt := int64(a.cfg.MaxPacketSize)
	if pkt <= 0 {
		pkt = 1
	}
	rttMS := int64(a.cfg.RTT / 1_000_000)
	if rttMS <= 0 {
		rttMS = 1
	}
	bdp := a.cfg.BandwidthBps * rttMS / 8000 // bits -> bytes over RTT
	w := int((bdp + pkt - 1) / pkt)
	if w < 1 {
		w = 1
	}
	if w > a.cfg.MaxWindowPackets {
		w = a.cfg.MaxWindowPackets
	}
	return w
}

// Handle accepts one packet. Identical repeats are idempotent; same sequence
// with conflicting identity/location/checksum is rejected.
func (a *Assembler) Handle(p Packet) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stats.Received++
	if a.cfg.StreamID != "" && p.StreamID != a.cfg.StreamID {
		return model.New(model.CodeInvalidArgument, model.StageTransport, "packet stream mismatch").
			WithSource(p.StreamID)
	}
	if len(p.Data) > a.cfg.MaxPacketSize {
		return model.New(model.CodePacketTooLarge, model.StageTransport, "packet exceeds size limit").
			WithOffset(p.Offset)
	}
	if err := p.ValidateChecksum(); err != nil {
		return err
	}
	fp := p.Fingerprint()
	if prior, ok := a.bySeq[p.Seq]; ok {
		if prior == fp {
			a.stats.Duplicate++
			return nil
		}
		return model.New(model.CodeDuplicateConflict, model.StageTransport,
			"conflicting packet with same stream and sequence").WithOffset(p.Offset)
	}

	a.bySeq[p.Seq] = fp
	a.seen[fp] = struct{}{}
	a.stats.Unique++
	if !a.haveHigh || p.Seq > a.highest {
		a.highest, a.haveHigh = p.Seq, true
	}

	switch {
	case p.Seq < a.nextSeq:
		a.stats.Duplicate++
		return nil
	case p.Seq == a.nextSeq:
		if p.Offset != a.nextOff {
			return model.New(model.CodeIntegrity, model.StageTransport, "contiguous packet offset mismatch").
				WithDetectedOffset(a.nextOff, p.Offset)
		}
		a.commitLocked(p)
		return a.flushLocked()
	default:
		if _, exists := a.buffered[p.Seq]; exists {
			a.stats.Duplicate++
			return nil
		}
		if len(a.buffered) >= a.cfg.MaxBufferedPackets ||
			a.bufBytes+int64(len(p.Data)) > a.cfg.MaxBufferedBytes {
			return model.New(model.CodeBufferLimit, model.StageTransport, "reorder buffer limit exceeded").
				WithOffset(p.Offset)
		}
		a.buffered[p.Seq] = p
		a.bufBytes += int64(len(p.Data))
		a.stats.Reordered++
		a.addGapLocked(p.Seq)
		return nil
	}
}

func (a *Assembler) addGapLocked(until uint64) {
	var added []uint64
	for s := a.nextSeq; s < until; s++ {
		if _, buffered := a.buffered[s]; buffered {
			continue
		}
		if _, ok := a.missing[s]; !ok {
			a.missing[s] = &missingState{}
			added = append(added, s)
		}
	}
	if len(added) >= a.cfg.BurstThreshold {
		for _, s := range added {
			a.missing[s].burst = true
		}
		a.stats.BurstLoss += len(added)
	}
}

func (a *Assembler) commitLocked(p Packet) {
	a.out = append(a.out, p.Data...)
	a.nextSeq++
	a.nextOff += int64(len(p.Data))
	delete(a.missing, p.Seq)
}

func (a *Assembler) flushLocked() error {
	for {
		p, ok := a.buffered[a.nextSeq]
		if !ok {
			return nil
		}
		if p.Offset != a.nextOff {
			return model.New(model.CodeIntegrity, model.StageTransport, "reordered packet offset mismatch").
				WithDetectedOffset(a.nextOff, p.Offset)
		}
		delete(a.buffered, a.nextSeq)
		a.bufBytes -= int64(len(p.Data))
		a.commitLocked(p)
	}
}

// RetransmitRequests returns at most the current window's missing sequences.
// Beyond MaxRetries it returns CodeRetryExhausted and refuses completion.
func (a *Assembler) RetransmitRequests() ([]uint64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	window := a.windowLocked()
	seqs := make([]uint64, 0, len(a.missing))
	for s := range a.missing {
		seqs = append(seqs, s)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	var out, exhausted []uint64
	for _, s := range seqs {
		st := a.missing[s]
		if st.retries >= a.cfg.MaxRetries {
			st.retries++
			a.stats.Exhausted++
			exhausted = append(exhausted, s)
			continue
		}
		if len(out) < window {
			st.retries++
			a.stats.Retransmit++
			out = append(out, s)
		}
	}
	if len(exhausted) > 0 {
		return out, model.New(model.CodeRetryExhausted, model.StageTransport,
			fmt.Sprintf("retransmission limit exceeded for seq %d", exhausted[0]))
	}
	return out, nil
}

// Assemble returns the complete stream only after totalPackets contiguous
// packets have committed and no gap or buffered packet remains.
func (a *Assembler) Assemble(totalPackets int) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.nextSeq != uint64(totalPackets) || len(a.missing) != 0 || len(a.buffered) != 0 {
		return nil, model.New(model.CodeIncomplete, model.StageTransport, "media packets incomplete").
			WithOffset(a.nextOff)
	}
	return append([]byte(nil), a.out...), nil
}
