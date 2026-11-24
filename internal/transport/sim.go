package transport

import (
	"sort"

	"github.com/pion/rtp"

	"mediaflow/internal/clock"
)

// Simulation describes a deterministic impairment profile. All randomness is
// drawn from the caller's seeded generator, so identical seeds reproduce
// identical statistics.
type Simulation struct {
	LossFraction      float64
	ReorderFraction   float64
	MaxReorder        int
	DuplicateFraction float64
	// OutOfWindowFraction makes a packet arrive after the receiver has already
	// given up on its position, so it is counted as unrecoverable rather than
	// recovered.
	OutOfWindowFraction float64
	// BaseDelayUs is the nominal transport delay applied to every packet.
	BaseDelayUs int64
}

// Rand is the subset of math/rand.Rand used here, kept as an interface so the
// simulation is testable with a fixed sequence.
type Rand interface {
	Float64() float64
	Intn(n int) int
}

// SimResult is the deterministic outcome of one simulated transmission.
type SimResult struct {
	Stats              Stats
	LostSequences      []uint16
	OutOfWindowSeq     []uint16
	DeliveredSequences []uint16
	DeliveredSet       map[uint16]bool
}

// Simulate delivers envs to a receiver with the impairment profile applied.
func Simulate(envs []Envelope, windowSize int, clockRate int64, sim Simulation, rng Rand) SimResult {
	recv := NewReceiver(windowSize, clockRate)
	type arrival struct {
		pkt *rtp.Packet
		at  int64
	}
	arrivals := make([]arrival, 0, len(envs))
	var sentSeq0 uint16
	if len(envs) > 0 {
		sentSeq0 = envs[0].Packet.SequenceNumber
	}
	spacing := envelopeSpacingUs(envs)
	delay := sim.BaseDelayUs
	if delay <= 0 {
		delay = 5_000
	}
	for _, env := range envs {
		base := env.MediaUs + delay
		if sim.LossFraction > 0 && rng.Float64() < sim.LossFraction {
			continue
		}
		if sim.OutOfWindowFraction > 0 && rng.Float64() < sim.OutOfWindowFraction {
			arrivals = append(arrivals, arrival{env.Packet, base + int64(windowSize+4)*spacing})
			continue
		}
		if sim.ReorderFraction > 0 && sim.MaxReorder > 0 && rng.Float64() < sim.ReorderFraction {
			base += int64(rng.Intn(sim.MaxReorder)+1) * spacing
		}
		if sim.DuplicateFraction > 0 && rng.Float64() < sim.DuplicateFraction {
			arrivals = append(arrivals, arrival{env.Packet, base})
		}
		arrivals = append(arrivals, arrival{env.Packet, base})
	}
	sort.SliceStable(arrivals, func(i, j int) bool { return arrivals[i].at < arrivals[j].at })
	for _, a := range arrivals {
		recv.Push(a.pkt, a.at)
	}
	stats := recv.Finish(sentSeq0, len(envs))
	delivered := recv.DeliveredSet()
	res := SimResult{Stats: stats, DeliveredSequences: recv.Delivered(), DeliveredSet: delivered}
	for i := 0; i < len(envs); i++ {
		seq := sentSeq0 + uint16(i)
		if delivered[seq] {
			continue
		}
		if recv.arrivals[seq] > 0 {
			res.OutOfWindowSeq = append(res.OutOfWindowSeq, seq)
		} else {
			res.LostSequences = append(res.LostSequences, seq)
		}
	}
	return res
}

func envelopeSpacingUs(envs []Envelope) int64 {
	if len(envs) < 2 {
		return 1_000
	}
	d := envs[1].MediaUs - envs[0].MediaUs
	if d <= 0 {
		d = 1_000
	}
	return d
}

// MissingWindows maps never-delivered packets to unified media windows so the
// pipeline can mark unrecoverable media explicitly.
func MissingWindows(envs []Envelope, delivered map[uint16]bool, frameUs int64) clock.Coverage {
	if frameUs <= 0 {
		frameUs = 33_000
	}
	var ivs []clock.Interval
	for _, env := range envs {
		if delivered[env.Packet.SequenceNumber] {
			continue
		}
		end := env.MediaUs + frameUs
		if env.DurationUs > 0 {
			end = env.MediaUs + env.DurationUs
		}
		ivs = append(ivs, clock.Interval{StartUs: env.MediaUs, EndUs: end})
	}
	return clock.Merge(ivs)
}
