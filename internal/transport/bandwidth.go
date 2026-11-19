package transport

import "math"

// BandwidthController chooses a reorder-buffer bound from observed bandwidth so
// latency cannot grow without limit while a weak uplink is repaired.
//
// The model keeps a target playout delay in milliseconds: lower bandwidth means
// more packets queue while retransmits are hoped for, so the bound shrinks to
// cap delay; recovering bandwidth lets the bound grow again.
type BandwidthController struct {
	MinMBps      float64
	MaxMBps      float64
	// MaxDelayMs is the largest playout delay the controller will allow.
	MaxDelayMs   float64
	// PktIntervalMs is the nominal spacing of packets at the stream rate.
	PktIntervalMs float64
}

// NewController builds a controller with sane defaults for proxy workloads.
func NewController() *BandwidthController {
	return &BandwidthController{
		MinMBps:       0.5,
		MaxMBps:       100,
		MaxDelayMs:    500,
		PktIntervalMs: 1,
	}
}

// MaxBuffered derives a packet-count bound for a bandwidth sample. A zero or
// negative sample is clamped to the floor; the bound never reaches zero so the
// pipeline can still make progress.
func (c *BandwidthController) MaxBuffered(mbps float64) int {
	if c == nil {
		return DefaultLimits().MaxBuffered
	}
	if mbps < c.MinMBps {
		mbps = c.MinMBps
	}
	if mbps > c.MaxMBps {
		mbps = c.MaxMBps
	}
	// Packets counted in one max-delay window. Higher bandwidth packs more
	// packets into the same delay, but the retransmit window can be shorter
	// because losses heal faster, so the bound is sub-linear.
	window := c.MaxDelayMs / math.Max(c.PktIntervalMs, 0.001)
	perWindow := window * math.Sqrt(mbps) / 20
	bound := int(perWindow)
	if bound < 8 {
		bound = 8
	}
	if limit := DefaultLimits().MaxBuffered * 4; bound > limit {
		bound = limit
	}
	return bound
}

// PlayoutDelay estimates the current buffering delay for a queue size and
// bandwidth sample, in milliseconds. It is used to report P95 latency.
func (c *BandwidthController) PlayoutDelay(queued int, mbps float64) float64 {
	if queued <= 0 {
		return 0
	}
	if mbps < c.MinMBps {
		mbps = c.MinMBps
	}
	spacing := c.PktIntervalMs
	if mbps > 0 {
		spacing = c.PktIntervalMs * math.Sqrt(c.MinMBps/mbps)
	}
	return float64(queued) * spacing
}

// Recommends reports whether an observed bandwidth sample forces a buffer
// shrink to protect latency, or permits growth after recovery.
func (c *BandwidthController) Recommends(current, sample int, mbps float64) (target int, shrink bool) {
	target = c.MaxBuffered(mbps)
	return target, target < current
}
