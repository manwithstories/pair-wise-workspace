package transport

import (
	"math/rand/v2"
	"sort"
)

// Action enumerates the impairment operations a trace step can schedule.
type Action uint8

const (
	ActPass Action = iota
	ActDuplicate
	ActReorder
	ActBurstDrop
	ActBandwidthStep
)

// Step is one deterministic trace event.
type Step struct {
	At     uint64 // simulated tick at which the event becomes active
	Action Action
	Count  uint32
	// DelayTick is the reorder delay applied to ActReorder packets.
	DelayTick uint64
	// BandwidthKbps is the new ceiling after ActBandwidthStep.
	BandwidthKbps uint32
}

// BandwidthPoint records one stepped bandwidth change for reporting.
type BandwidthPoint struct {
	At           uint64
	BandwidthKbps uint32
}

// Trace is a fully deterministic impairment schedule.
type Trace struct {
	Seed     uint64
	Steps    []Step
	BaseKbps uint32
}

// NewTrace builds the canonical trace used by fixtures and the CLI. The
// layout is fixed: duplicates, reordering, four burst windows and bandwidth
// stairs, all positioned at absolute packet ticks.
func NewTrace(seed uint64, packets uint64) *Trace {
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	tr := &Trace{
		Seed: seed, BaseKbps: 2000,
	}
	// 24 duplicates spread deterministically across the stream.
	for i := 0; i < 24; i++ {
		at := uint64(rng.Uint64N(packets-32)) + 8
		tr.Steps = append(tr.Steps, Step{At: at, Action: ActDuplicate, Count: 1})
	}
	// 16 reorder events: hold a packet and release it a few ticks later.
	for i := 0; i < 16; i++ {
		at := uint64(rng.Uint64N(packets-64)) + 16
		tr.Steps = append(tr.Steps, Step{
			At: at, Action: ActReorder, Count: 1,
			DelayTick: uint64(6 + rng.Uint64N(10)),
		})
	}
	// 5 burst windows: 4 packets each = 20 dropped. Retransmission can
	// recover 16 of them inside the playout deadline; 4 expire.
	for i := 0; i < 5; i++ {
		at := uint64(128) + packets*uint64(i)/6
		tr.Steps = append(tr.Steps, Step{At: at, Action: ActBurstDrop, Count: 4})
	}
	// 4 bandwidth stairs.
	for i := 1; i <= 4; i++ {
		tr.Steps = append(tr.Steps, Step{
			At:     packets * uint64(i) / 5,
			Action: ActBandwidthStep,
			BandwidthKbps: 2000 - uint32(i)*350,
		})
	}
	sort.Slice(tr.Steps, func(i, j int) bool { return tr.Steps[i].At < tr.Steps[j].At })
	return tr
}

// TraceCounters summarizes what the trace did. The engine drives updates from
// a single goroutine; Snapshot returns a defensive copy for report readers.
type TraceCounters struct {
	Duplicates uint64
	Reordered  uint64
	BurstDrops uint64
	BwSteps    []BandwidthPoint
}

// Snapshot returns a copy safe to retain after the run.
func (c *TraceCounters) Snapshot() (dup, reo, drop uint64, bw []BandwidthPoint) {
	return c.Duplicates, c.Reordered, c.BurstDrops,
		append([]BandwidthPoint(nil), c.BwSteps...)
}
