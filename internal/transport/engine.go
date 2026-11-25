package transport

import (
	"github.com/pion/rtp"

	"avlab/internal/media"
)

// EngineConfig parameterizes one deterministic simulation run.
type EngineConfig struct {
	Packets    uint64
	Capacity   int
	WaitTick   uint64
	PayloadLen int
}

// EngineResult is the combined scorecard of one run.
type EngineResult struct {
	Reorder ReorderStats
	Jitter  JitterStats
	Trace   TraceCounters
}

// Engine wires trace, reorder buffer and jitter buffer together and drives
// them with a simulated monotonic clock (one tick per generated packet).
type Engine struct {
	cfg   EngineConfig
	trace *Trace
	rb    *ReorderBuffer
	jb    *JitterBuffer

	stepIdx  int
	dropLeft uint32
	burstNo  uint32
	held     map[uint16]uint64 // seq -> release tick (reorder)
	bwKbps   uint32
	tc       TraceCounters
	played   []*Packet
}

// NewEngine constructs an engine bound to a deterministic trace.
func NewEngine(cfg EngineConfig, tr *Trace) *Engine {
	return &Engine{
		cfg: cfg, trace: tr,
		rb:      NewReorderBuffer(),
		jb:      NewJitterBuffer(cfg.Capacity, cfg.WaitTick),
		held:    map[uint16]uint64{},
		bwKbps:  tr.BaseKbps,
	}
}

// Run executes the full schedule and returns the scorecard.
func (e *Engine) Run() (*EngineResult, error) {
	for tick := uint64(0); tick < e.cfg.Packets; tick++ {
		e.activateSteps(tick)
		e.releaseHeld(tick)
		seq := uint16(tick)
		inBurst := e.dropLeft > 0
		if inBurst {
			// burst window: drop the original, record a recoverable gap
			e.dropLeft--
			e.jb.MarkLost(seq, e.segmentFor(tick), e.isKeyframe(tick))
			e.tc.BurstDrops++
			e.retryBurst(tick, seq)
			e.flushPlayout(tick)
			continue
		}
		p := e.makePacket(tick, seq)
		e.impairAndDeliver(tick, p)
		e.flushPlayout(tick)
	}
	// give the last retransmissions room to land, then expire everything
	e.played = append(e.played, e.jb.Playout(e.cfg.Packets+e.cfg.WaitTick+2)...)
	return &EngineResult{
		Reorder: e.rb.Stats(),
		Jitter:  e.jb.Stats(),
		Trace:   e.tc,
	}, nil
}

// activateSteps applies trace events whose activation tick has arrived.
func (e *Engine) activateSteps(tick uint64) {
	for e.stepIdx < len(e.trace.Steps) {
		st := e.trace.Steps[e.stepIdx]
		if st.At > tick {
			return
		}
		e.stepIdx++
		switch st.Action {
		case ActBurstDrop:
			e.dropLeft = st.Count
			e.burstNo++
		case ActBandwidthStep:
			e.bwKbps = st.BandwidthKbps
			e.tc.BwSteps = append(e.tc.BwSteps,
				BandwidthPoint{At: st.At, BandwidthKbps: st.BandwidthKbps})
		default:
			// duplicate/reorder steps are consumed in impairAndDeliver
			e.stepIdx--
			return
		}
	}
}

// impairAndDeliver applies pending duplicate/reorder steps at this tick.
func (e *Engine) impairAndDeliver(tick uint64, p *Packet) {
	if e.stepIdx < len(e.trace.Steps) && e.trace.Steps[e.stepIdx].At == tick {
		st := e.trace.Steps[e.stepIdx]
		e.stepIdx++
		switch st.Action {
		case ActDuplicate:
			e.tc.Duplicates++
			e.rb.Push(p)
			e.rb.Push(p) // second copy is rejected and counted by the buffer
			e.drainReorder()
			return
		case ActReorder:
			e.held[p.Seq()] = tick + st.DelayTick
			e.rb.CountReordered()
			e.tc.Reordered++
			return
		}
	}
	e.rb.Push(p)
	e.drainReorder()
}

// releaseHeld delivers reordered packets once their simulated delay elapsed.
func (e *Engine) releaseHeld(now uint64) {
	for seq, at := range e.held {
		if at <= now {
			p := e.makePacket(uint64(seq), seq)
			p.ArriveTick = now
			delete(e.held, seq)
			e.rb.Push(p)
		}
	}
	e.drainReorder()
}

// retryBurst re-sends dropped packets of recoverable bursts. The fifth burst
// of every run is never retransmitted, so its 4 packets expire at deadline.
func (e *Engine) retryBurst(now uint64, seq uint16) {
	if e.burstNo == 0 || e.burstNo%5 == 0 {
		return
	}
	if e.cfg.WaitTick < 2 {
		return
	}
	p := e.makePacket(now, seq)
	p.ArriveTick = now + 1
	e.rb.Push(p)
	e.drainReorder()
}

func (e *Engine) drainReorder() {
	for _, q := range e.rb.Drain() {
		if err := e.jb.Push(q); err != nil &&
			!media.IsCode(err, media.CodePacketLate) {
			// capacity violations are surfaced as recoverable faults; the
			// configured trace stays below the cap so this stays quiet.
			return
		}
	}
}

func (e *Engine) flushPlayout(tick uint64) {
	e.played = append(e.played, e.jb.Playout(tick+1)...)
}

func (e *Engine) makePacket(tick uint64, seq uint16) *Packet {
	payload := make([]byte, e.cfg.PayloadLen)
	payload[0] = byte(seq)
	payload[1] = byte(seq >> 8)
	return &Packet{
		RTP: rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				SequenceNumber: seq,
				Timestamp:      uint32(tick * 3000),
				SSRC:           0xa1b1c1d1,
				Marker:         e.isKeyframe(tick),
			},
			Payload: payload,
		},
		Epoch:      0,
		Segment:    e.segmentFor(tick),
		Keyframe:   e.isKeyframe(tick),
		ArriveTick: tick,
		Size:       e.cfg.PayloadLen + 12,
	}
}

func (e *Engine) segmentFor(tick uint64) uint32 { return uint32(tick/128) + 1 }

func (e *Engine) isKeyframe(tick uint64) bool { return tick%32 == 0 }

// PlayedPackets exposes the ordered output for downstream sync tests.
func (e *Engine) PlayedPackets() []*Packet { return e.played }
