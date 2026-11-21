package rtp

import (
	"fmt"
	"math"
	"sort"

	pionrtp "github.com/pion/rtp"

	"mediad/internal/model"
)

func NewReceiver(cfg Config) (*Receiver, error) {
	def := DefaultConfig()
	if cfg.InitialWindow == 0 {
		cfg.InitialWindow = def.InitialWindow
	}
	if cfg.MinWindow == 0 {
		cfg.MinWindow = def.MinWindow
	}
	if cfg.MaxWindow == 0 {
		cfg.MaxWindow = def.MaxWindow
	}
	if cfg.MaxBuffered == 0 {
		cfg.MaxBuffered = def.MaxBuffered
	}
	if cfg.ReferenceBandwidthMbps <= 0 {
		cfg.ReferenceBandwidthMbps = def.ReferenceBandwidthMbps
	}
	if cfg.ClockRate == 0 {
		cfg.ClockRate = def.ClockRate
	}
	if cfg.MinWindow > cfg.MaxWindow {
		return nil, model.New(model.KindInvalidInput, "rtp.reorder", "min window exceeds max window")
	}
	if cfg.InitialWindow < cfg.MinWindow || cfg.InitialWindow > cfg.MaxWindow {
		return nil, model.New(model.KindInvalidInput, "rtp.reorder",
			fmt.Sprintf("initial window %d outside [%d,%d]", cfg.InitialWindow, cfg.MinWindow, cfg.MaxWindow))
	}
	if cfg.MaxBuffered < 1 {
		return nil, model.New(model.KindInvalidInput, "rtp.reorder", "max buffered must be positive")
	}
	r := &Receiver{
		cfg:       cfg,
		window:    cfg.InitialWindow,
		buffer:    make(map[uint64]*Packet, cfg.InitialWindow),
		lostSet:   make(map[uint64]bool),
		delivered: make(map[uint64]bool),
	}
	r.stats.WindowStart = cfg.InitialWindow
	return r, nil
}

// Window returns the current reorder window in packets.
func (r *Receiver) Window() int { return r.window }

// SetBandwidth adjusts the reorder window for the observed bandwidth. A
// slower link widens the window; the window never leaves its configured
// bounds and every change is recorded.
func (r *Receiver) SetBandwidth(mbps float64) {
	if mbps <= 0 {
		return
	}
	w := int(math.Round(float64(r.cfg.InitialWindow) * r.cfg.ReferenceBandwidthMbps / mbps))
	if w < r.cfg.MinWindow {
		w = r.cfg.MinWindow
	}
	if w > r.cfg.MaxWindow {
		w = r.cfg.MaxWindow
	}
	if w == r.window {
		return
	}
	r.stats.WindowShifts = append(r.stats.WindowShifts, WindowShift{
		AtMbps: mbps, From: r.window, To: w, Buffered: len(r.buffer),
	})
	r.window = w
}

// unwrap maps a 16-bit sequence number onto a monotonically increasing
// 64-bit sequence. The first packet seen anchors the stream at 0 and every
// later packet is placed at its signed 16-bit distance from the highest
// sequence seen so far, which makes wraps exact.
func (r *Receiver) unwrap(seq uint16) uint64 {
	if !r.started {
		r.started = true
		r.highestSeq = seq
		r.highestExt = 0
		return 0
	}
	diff := int32(int16(seq - r.highestSeq))
	if diff > 0 {
		r.highestExt += uint64(diff)
		r.highestSeq = seq
		return r.highestExt
	}
	if uint64(-diff) > r.highestExt {
		// Further behind than the stream is long; clamp to the start.
		return 0
	}
	return r.highestExt - uint64(-diff)
}

// Push accepts one packet and returns packets that became deliverable, in
// stream order.
func (r *Receiver) Push(p *pionrtp.Packet) ([]Packet, error) {
	if p == nil {
		return nil, model.New(model.KindInvalidInput, "rtp.reorder", "nil packet")
	}
	r.stats.Received++
	arrival := r.arrived
	r.arrived++

	seq := r.unwrap(p.SequenceNumber)
	if r.delivered[seq] {
		r.stats.Duplicates++
		return nil, nil
	}
	if r.lostSet[seq] {
		r.stats.TooLate++
		return nil, nil
	}
	if seq < r.next {
		r.stats.Duplicates++
		return nil, nil
	}
	if _, ok := r.buffer[seq]; ok {
		r.stats.Duplicates++
		return nil, nil
	}
	if len(r.buffer) >= r.cfg.MaxBuffered {
		return nil, model.New(model.KindOverLimit, "rtp.reorder",
			fmt.Sprintf("reorder buffer holds %d packets, limit %d", len(r.buffer), r.cfg.MaxBuffered))
	}
	pkt := &Packet{
		Seq:          seq,
		RTPSeq:       p.SequenceNumber,
		Timestamp:    p.Timestamp,
		Marker:       p.Marker,
		PayloadBytes: len(p.Payload),
		Arrival:      arrival,
		Reordered:    seq < r.maxSeen,
	}
	r.buffer[seq] = pkt
	if seq > r.maxSeen {
		r.maxSeen = seq
	}
	return r.drain(false), nil
}

// Flush releases everything still buffered, marking remaining gaps lost.
func (r *Receiver) Flush() []Packet {
	return r.drain(true)
}

// Stats returns a snapshot of the receiver's counters.
func (r *Receiver) Stats() Stats {
	s := r.stats
	s.WindowEnd = r.window
	s.MissingSeqs = append([]uint64(nil), r.stats.MissingSeqs...)
	s.WindowShifts = append([]WindowShift(nil), r.stats.WindowShifts...)
	sort.Slice(s.MissingSeqs, func(i, j int) bool { return s.MissingSeqs[i] < s.MissingSeqs[j] })
	return s
}
