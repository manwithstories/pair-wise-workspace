// Package fanout replicates normalized packets into 1x, 2x and 4x representations
// and models dynamic bandwidth-driven representation selection.
package fanout

import (
	"fmt"

	"medcheck/fault"
	"medcheck/ingest"
)

// Representation is a selectable output layer.
type Representation struct {
	ID         string `json:"id"`
	Multiplier int    `json:"multiplier"`
	BitrateBps int64  `json:"bitrate_bps"`
}

// Standard returns the 1x, 2x and 4x representations.
func Standard(baseBitrateBps int64) []Representation {
	return []Representation{
		{ID: "1x", Multiplier: 1, BitrateBps: baseBitrateBps},
		{ID: "2x", Multiplier: 2, BitrateBps: baseBitrateBps * 2},
		{ID: "4x", Multiplier: 4, BitrateBps: baseBitrateBps * 4},
	}
}

// Sink receives one fanned-out packet.
type Sink func(rep Representation, pkt ingest.Packet) error

// Fanout copies every packet to every requested representation. Payload slices
// are read-only in this pipeline, so copies share the underlying byte array.
func Fanout(packets []ingest.Packet, reps []Representation, sink Sink) error {
	for _, rep := range reps {
		for _, pkt := range packets {
			clone := pkt
			clone.Header.Stream = rep.ID
			if err := sink(rep, clone); err != nil {
				return fmt.Errorf("fanout layer %s seq=%d: %w", rep.ID, pkt.Seq, err)
			}
		}
	}
	return nil
}

// Controller tracks available bandwidth and sudden drops.
type Controller struct {
	reps       []Representation
	lastBps    int64
	hasLast    bool
	DropRatio  float64
	DropEvents int
	faults     *fault.Model
}

// NewController creates a bandwidth controller.
func NewController(reps []Representation, faults *fault.Model) *Controller {
	return &Controller{reps: reps, DropRatio: 0.5, faults: faults}
}

// Observe records a bandwidth sample. A fall of at least DropRatio is a sudden
// drop. It returns the representations that fit the new bandwidth.
func (c *Controller) Observe(atMS, bps int64) (drop bool, allowed []Representation) {
	if c.hasLast && c.lastBps > 0 {
		if float64(bps) <= float64(c.lastBps)*(1-c.DropRatio) {
			drop = true
			c.DropEvents++
			c.faults.Add(fault.Fault{
				Kind:    fault.BandwidthDrop,
				Span:    fault.Span{StartMS: atMS, EndMS: atMS},
				Message: fmt.Sprintf("bandwidth dropped %d -> %d bps", c.lastBps, bps),
			})
		}
	}
	c.lastBps = bps
	c.hasLast = true
	for _, rep := range c.reps {
		if rep.BitrateBps <= bps {
			allowed = append(allowed, rep)
		}
	}
	return drop, allowed
}

// Select returns all representations whose bitrate fits bps.
func Select(reps []Representation, bps int64) []Representation {
	var out []Representation
	for _, rep := range reps {
		if rep.BitrateBps <= bps {
			out = append(out, rep)
		}
	}
	return out
}
