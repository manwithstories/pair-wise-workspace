// Package rtp receives a packetised media stream, removes duplicates,
// restores order and reports unrecoverable loss. It never invents data: a
// frame that did not arrive is reported as a gap, never synthesised.
package rtp

import (
	"fmt"
	"math"
	"sort"

	pionrtp "github.com/pion/rtp"

	"mediad/internal/model"
)

// Config bounds the reorder buffer.
type Config struct {
	// InitialWindow is the number of sequence numbers the receiver may wait
	// behind the highest sequence seen before declaring a gap lost.
	InitialWindow int
	// MinWindow and MaxWindow clamp adaptive changes.
	MinWindow int
	MaxWindow int
	// MaxBuffered caps buffered packets. Exceeding it is an error, never a
	// silent drop.
	MaxBuffered int
	// ReferenceBandwidthMbps is the bandwidth at which InitialWindow applies.
	ReferenceBandwidthMbps float64
	// ClockRate is the RTP media clock in ticks per second.
	ClockRate uint32
}

// DefaultConfig returns safe defaults for a 90kHz video stream.
func DefaultConfig() Config {
	return Config{
		InitialWindow:          64,
		MinWindow:              8,
		MaxWindow:              2048,
		MaxBuffered:            8192,
		ReferenceBandwidthMbps: 8,
		ClockRate:              90000,
	}
}

// Packet is one de-duplicated packet in stream order.
type Packet struct {
	Seq          uint64
	RTPSeq       uint16
	Timestamp    uint32
	Marker       bool
	PayloadBytes int
	// Arrival is the 0-based index at which the packet was handed to Push.
	Arrival int
	// Reordered is true when the packet arrived after a later sequence
	// number but was still delivered in time.
	Reordered bool
}

// Stats quantifies what the receiver observed.
type Stats struct {
	Received     int           `json:"received"`
	Delivered    int           `json:"delivered"`
	Duplicates   int           `json:"duplicates"`
	Reordered    int           `json:"reordered"`
	Lost         int           `json:"lost"`
	TooLate      int           `json:"too_late"`
	MissingSeqs  []uint64      `json:"missing_sequences"`
	WindowStart  int           `json:"window_start"`
	WindowEnd    int           `json:"window_end"`
	WindowShifts []WindowShift `json:"window_shifts"`
}

// WindowShift records one adaptive window change.
type WindowShift struct {
	AtMbps   float64 `json:"at_mbps"`
	From     int     `json:"from"`
	To       int     `json:"to"`
	Buffered int     `json:"buffered"`
}

// Receiver reorders one RTP stream.
type Receiver struct {
	cfg Config

	started    bool
	next       uint64
	maxSeen    uint64
	highestSeq uint16
	highestExt uint64
	buffer     map[uint64]*Packet
	lostSet    map[uint64]bool
	delivered  map[uint64]bool
	arrived    int
	window     int

	stats Stats
}

// NewReceiver creates a receiver for cfg, applying defaults for zero fields.
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
