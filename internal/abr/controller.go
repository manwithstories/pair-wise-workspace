// Package abr estimates available bandwidth from sliding-window samples and
// decides which of the fixed representation tiers (180/450/900 kbps) each
// one-second segment is encoded at.
package abr

import "sort"

// TierKbps is the fixed set of representation bitrates.
var TierKbps = []int{180, 450, 900}

// MaxBitrateDeltaPct is the acceptance threshold for encoded results.
const MaxBitrateDeltaPct = 20.0

// Controller keeps a sliding window of per-segment bandwidth samples,
// expressed in bits per second.
type Controller struct {
	window   []int64 // bps
	capacity int
}

// NewController builds a controller with the given window capacity.
func NewController(windowCapacity int) *Controller {
	if windowCapacity < 1 {
		windowCapacity = 1
	}
	return &Controller{capacity: windowCapacity}
}

// Observe adds one bandwidth measurement and drops the oldest when full.
func (c *Controller) Observe(bps int64) {
	c.window = append(c.window, bps)
	if len(c.window) > c.capacity {
		c.window = c.window[len(c.window)-c.capacity:]
	}
}

// Estimate returns the conservative bandwidth estimate: the mean of the
// lower half of the window. With fewer than two samples the raw value is
// returned, clamped at zero.
func (c *Controller) Estimate() int64 {
	n := len(c.window)
	if n == 0 {
		return 0
	}
	sorted := append([]int64(nil), c.window...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	keep := (n + 1) / 2 // strictly the lower half, rounded up
	var sum int64
	for i := 0; i < keep; i++ {
		sum += sorted[i]
	}
	return sum / int64(keep)
}

// Decide selects the highest tier whose bitrate fits within the estimated
// bandwidth with 15% headroom for protocol overhead. It never exceeds the
// highest configured tier.
func (c *Controller) Decide() int {
	estimate := c.Estimate()
	chosen := TierKbps[0]
	for _, tier := range TierKbps {
		need := int64(tier*1000) * 115 / 100
		if estimate >= need {
			chosen = tier
		}
	}
	return chosen
}

// BitrateDeltaPct compares the achieved segment bitrate with the target.
// Positive means the encoder overshot the target.
func BitrateDeltaPct(targetKbps int, actualBps int64) float64 {
	targetBps := int64(targetKbps) * 1000
	if targetBps == 0 {
		return 0
	}
	return (float64(actualBps-targetBps) / float64(targetBps)) * 100
}

// WithinTolerance reports whether an achieved bitrate passes the 20% rule.
func WithinTolerance(targetKbps int, actualBps int64) bool {
	d := BitrateDeltaPct(targetKbps, actualBps)
	return d <= MaxBitrateDeltaPct && d >= -MaxBitrateDeltaPct
}
