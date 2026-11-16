// Package fixture builds deterministic, self-contained RTP sample streams.
//
// Everything the demo sends is produced here from a fixed seed, so a run is
// reproducible without any external media file. Faults are injected by
// scheduling decisions on a virtual timeline rather than by sleeping: the
// generator emits a list of SendEvent values and the caller decides how to
// pace them in real time.
package fixture

import (
	"errors"
	"fmt"
	"math/rand"

	"github.com/pion/rtp"
)

// DefaultClockRate is the 90 kHz video clock RTP uses for timestamp pacing.
const DefaultClockRate = 90000

// ErrInvalidConfig is returned when the generator cannot honour a setting.
var ErrInvalidConfig = errors.New("invalid fixture configuration")

// Config describes the sample stream to generate.
type Config struct {
	// SSRC is the fixed synchronization source. Requirement 4 pins it.
	SSRC uint32
	// Packets is the number of source packets before loss injection.
	Packets int
	// IntervalMs is the intended gap between packets.
	IntervalMs int
	// ReorderWindow is the maximum number of slots a packet may be delayed.
	ReorderWindow int
	// ReorderRate is the fraction of packets delayed within the window.
	ReorderRate float64
	// DuplicateRate is the fraction of packets sent twice.
	DuplicateRate float64
	// LossRate is the fraction of source packets dropped at the sender.
	LossRate float64
	// Seed makes the whole schedule deterministic.
	Seed int64
	// ClockRate is the RTP timestamp clock (90000 for video).
	ClockRate int
	// StartSequence is the first sequence number, defaulting to a value near
	// the wraparound point so requirement 5 is exercised by default.
	StartSequence uint16
}

// Defaults that describe the demo command's shape.
const (
	DefaultStartSequence = 65500
	DefaultPayloadSize   = 96
)

// Validate normalizes and checks the configuration.
func (c *Config) Validate() error {
	if c.Packets <= 0 {
		return fmt.Errorf("%w: packets must be > 0", ErrInvalidConfig)
	}
	if c.IntervalMs <= 0 {
		return fmt.Errorf("%w: interval-ms must be > 0", ErrInvalidConfig)
	}
	if c.ReorderWindow < 0 {
		return fmt.Errorf("%w: reorder window must be >= 0", ErrInvalidConfig)
	}
	if c.ReorderWindow > 4096 {
		return fmt.Errorf("%w: reorder window too large (%d)", ErrInvalidConfig, c.ReorderWindow)
	}
	for name, rate := range map[string]float64{
		"reorder-rate":   c.ReorderRate,
		"duplicate-rate": c.DuplicateRate,
		"loss-rate":      c.LossRate,
	} {
		if rate < 0 || rate > 1 {
			return fmt.Errorf("%w: %s must be within [0,1], got %v", ErrInvalidConfig, name, rate)
		}
	}
	if c.ClockRate == 0 {
		c.ClockRate = DefaultClockRate
	}
	return nil
}

// SourcePacket is one packet as the generator intends to send it.
type SourcePacket struct {
	Sequence  uint16
	Timestamp uint32
	Marker    bool
	Index     int
	Duplicate bool
	// OffsetMs is the virtual send time relative to the first packet.
	OffsetMs int64
}

// Plan is the complete deterministic schedule, separated into the packets
// that belong on the wire and the statistics the generator predicts.
type Plan struct {
	SSRC      uint32         `json:"ssrc"`
	ClockRate int            `json:"clock_rate"`
	Source    int            `json:"source_packets"`
	Send      []SourcePacket `json:"-"`
	Expected  ExpectedCounts `json:"expected"`
	StartSeq  uint16         `json:"start_sequence"`
	EndSeq    uint16         `json:"end_sequence"`
	Duration  int64          `json:"duration_ms"`
}

// ExpectedCounts is what the generator itself injected. Tests compare the
// receiver metrics against these numbers, so they must be exact rather than
// probabilistic.
type ExpectedCounts struct {
	Dropped    int `json:"dropped"`
	Duplicated int `json:"duplicated"`
	Reordered  int `json:"reordered"`
	Sent       int `json:"sent"`
	Unique     int `json:"unique"`
}

// Build turns cfg into a deterministic send plan.
//
// The schedule obeys three rules:
//   - loss is decided first, so a dropped packet never also becomes a
//     duplicate or a reorder;
//   - a reordered packet is delayed by 1..ReorderWindow slots, which may move
//     it past neighbours and therefore out of order without changing its
//     sequence number;
//   - a duplicate is emitted 1-3 slots after its original, so the receiver
//     observes it as a true duplicate rather than an in-order repeat.
func Build(cfg Config) (*Plan, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	rng := rand.New(rand.NewSource(cfg.Seed))
	if cfg.StartSequence == 0 {
		cfg.StartSequence = DefaultStartSequence
	}
	plan := &Plan{
		SSRC:      cfg.SSRC,
		ClockRate: cfg.ClockRate,
		Source:    cfg.Packets,
		StartSeq:  cfg.StartSequence,
	}
	tsStep := uint32(float64(cfg.ClockRate) * float64(cfg.IntervalMs) / 1000.0)
	if tsStep == 0 {
		tsStep = 1
	}

	// offsetSlots holds the virtual send slot per source packet, so a delay
	// can push a packet past later ones.
	slots := make([]int64, cfg.Packets)
	var dupSlots []int64
	for i := 0; i < cfg.Packets; i++ {
		slot := int64(i)
		if cfg.LossRate > 0 && rng.Float64() < cfg.LossRate {
			slots[i] = -1
			continue
		}
		if cfg.ReorderWindow > 0 && cfg.ReorderRate > 0 && rng.Float64() < cfg.ReorderRate {
			slot += int64(1 + rng.Intn(cfg.ReorderWindow))
		}
		slots[i] = slot
		if cfg.DuplicateRate > 0 && rng.Float64() < cfg.DuplicateRate {
			dupSlots = append(dupSlots, slot+int64(1+rng.Intn(3)))
			plan.Expected.Duplicated++
		}
	}

	for i := 0; i < cfg.Packets; i++ {
		if slots[i] < 0 {
			continue
		}
		plan.Send = append(plan.Send, SourcePacket{
			Sequence:  cfg.StartSequence + uint16(i),
			Timestamp: uint32(i) * tsStep,
			Marker:    i%30 == 0,
			Index:     i,
			OffsetMs:  slots[i] * int64(cfg.IntervalMs),
		})
	}
	for _, slot := range dupSlots {
		// A duplicate re-sends the most recent original at or before its slot,
		// keeping the original's sequence number and timestamp exactly, which
		// is what makes it detectable as a duplicate on the wire.
		src := nearestOriginal(plan.Send, slot)
		if src == nil {
			continue
		}
		cp := *src
		cp.Duplicate = true
		cp.OffsetMs = slot * int64(cfg.IntervalMs)
		plan.Send = append(plan.Send, cp)
	}
	sortByOffset(plan.Send)
	// Predictions are measured from the finished schedule, not from the
	// decisions that built it: a delayed packet whose delay lands it behind no
	// other packet is in order on the wire and must not be counted as
	// reordered.
	plan.Expected = measureSchedule(plan.Send, cfg.Packets)
	if n := len(plan.Send); n > 0 {
		plan.Duration = plan.Send[n-1].OffsetMs
	}
	if plan.Expected.Unique > 0 {
		plan.EndSeq = cfg.StartSequence + uint16(cfg.Packets-1)
	}
	return plan, nil
}

// measureSchedule derives the exact counts the receiver should observe.
func measureSchedule(sent []SourcePacket, sourcePackets int) ExpectedCounts {
	var counts ExpectedCounts
	counts.Sent = len(sent)
	var (
		highest   uint64
		haveFirst bool
		seen      = make(map[uint64]struct{})
	)
	for _, p := range sent {
		if p.Duplicate {
			continue
		}
		ext := uint64(p.Sequence)
		if haveFirst {
			base := highest &^ 0xFFFF
			ext = base | uint64(p.Sequence)
			if diff := int64(ext) - int64(highest); diff < -32768 {
				ext += 1 << 16
			}
		}
		if _, dup := seen[ext]; dup {
			continue
		}
		seen[ext] = struct{}{}
		counts.Unique++
		if haveFirst && ext < highest {
			counts.Reordered++
		}
		if !haveFirst || ext > highest {
			highest = ext
		}
		haveFirst = true
	}
	counts.Dropped = sourcePackets - counts.Unique
	counts.Duplicated = counts.Sent - counts.Unique
	return counts
}

// nearestOriginal finds the non-duplicate packet whose slot is closest to,
// and not after, slot. It is a tiny linear scan because plans are small.
func nearestOriginal(sent []SourcePacket, slot int64) *SourcePacket {
	var best *SourcePacket
	for i := range sent {
		if sent[i].Duplicate || sent[i].OffsetMs > slot {
			continue
		}
		if best == nil || sent[i].OffsetMs >= best.OffsetMs {
			best = &sent[i]
		}
	}
	return best
}

func sortByOffset(p []SourcePacket) {
	// Insertion into the existing order is fine: plans are at most a few
	// thousand entries, and a stable sort keeps duplicates after originals.
	for i := 1; i < len(p); i++ {
		for j := i; j > 0 && p[j].OffsetMs < p[j-1].OffsetMs; j-- {
			p[j], p[j-1] = p[j-1], p[j]
		}
	}
}

// Packet materializes one source packet as a Pion RTP packet. The payload is
// derived from the sequence number, so a receiver can verify the bytes were
// forwarded untouched.
func (s SourcePacket) Packet(ssrc uint32) *rtp.Packet {
	payload := make([]byte, DefaultPayloadSize)
	for i := range payload {
		payload[i] = byte(s.Sequence) + byte(i)
	}
	return &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: s.Sequence,
			Timestamp:      s.Timestamp,
			SSRC:           ssrc,
			Marker:         s.Marker,
		},
		Payload: payload,
	}
}
