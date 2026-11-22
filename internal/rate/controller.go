// Package rate computes target bitrate decisions against a time-varying
// bandwidth trace and measures the achieved bitrate from delivered frames.
package rate

import (
	"fmt"

	"mediascan/internal/clock"
)

// Sample is one bandwidth observation in kbps at a media tick.
type Sample struct {
	Tick      int64
	Bandwidth int // kbps available on the link
}

// Decision is the controller output for one frame slot.
type Decision struct {
	Tick        int64
	Bandwidth   int // observed available kbps
	TargetKbps  int // controller target for this slot
	ActualKbps  int // achieved from delivered bytes (0 when frame lost)
	ErrorPctX10 int // |actual-target|/target * 100, fixed point *100
}

// Config parameterises the AIMD-ish controller.
type Config struct {
	// FloorKbps / CeilKbps bound every target.
	FloorKbps int
	CeilKbps  int
	// Headroom is the fraction of bandwidth reserved for overhead (0.15).
	Headroom float64
	// IncreaseKbps is the additive step per slot when under-utilising;
	// DecreaseFactor is the multiplicative cut on congestion.
	IncreaseKbps   int
	DecreaseFactor float64
}

// DefaultConfig returns conservative mobile-link parameters.
func DefaultConfig(target int) Config {
	return Config{
		FloorKbps:      150,
		CeilKbps:       2500,
		Headroom:       0.15,
		IncreaseKbps:   25,
		DecreaseFactor: 0.8,
	}
}

// Trace is a piecewise-constant bandwidth trajectory.
type Trace struct {
	clk     clock.Clock
	samples []Sample
}

// NewTrace validates and stores a monotonically ticked bandwidth trace.
func NewTrace(clk clock.Clock, samples []Sample) (*Trace, error) {
	if len(samples) == 0 {
		return nil, fmt.Errorf("rate: empty bandwidth trace")
	}
	prev := int64(-1)
	for _, s := range samples {
		if s.Tick < 0 || s.Tick <= prev {
			return nil, fmt.Errorf("rate: trace ticks must be strictly increasing")
		}
		if s.Bandwidth <= 0 {
			return nil, fmt.Errorf("rate: bandwidth must be positive at tick %d", s.Tick)
		}
		prev = s.Tick
	}
	return &Trace{clk: clk, samples: samples}, nil
}

// BandwidthAt returns the piecewise-constant bandwidth for a media tick.
func (t *Trace) BandwidthAt(tick int64) int {
	bw := t.samples[0].Bandwidth
	for _, s := range t.samples {
		if s.Tick > tick {
			break
		}
		bw = s.Bandwidth
	}
	return bw
}

// Controller walks frame slots and emits per-slot decisions.
type Controller struct {
	cfg    Config
	trace  *Trace
	clk    clock.Clock
	target int
}

// NewController seeds the controller at the configured starting target.
func NewController(cfg Config, trace *Trace, clk clock.Clock, startTarget int) (*Controller, error) {
	if cfg.FloorKbps <= 0 || cfg.CeilKbps < cfg.FloorKbps {
		return nil, fmt.Errorf("rate: invalid floor/ceiling")
	}
	if startTarget < cfg.FloorKbps || startTarget > cfg.CeilKbps {
		return nil, fmt.Errorf("rate: start target %d outside [%d,%d]", startTarget, cfg.FloorKbps, cfg.CeilKbps)
	}
	return &Controller{cfg: cfg, trace: trace, clk: clk, target: startTarget}, nil
}

// Step produces the decision for one frame slot given the bytes delivered
// (0 means the frame was lost) and advances the internal target.
func (c *Controller) Step(frameIdx int, deliveredBytes int) (Decision, error) {
	if deliveredBytes < 0 {
		return Decision{}, fmt.Errorf("rate: negative delivered bytes")
	}
	tick, err := c.clk.FrameTicks(int64(frameIdx))
	if err != nil {
		return Decision{}, err
	}
	bw := c.trace.BandwidthAt(tick)
	afford := int(float64(bw) * (1 - c.cfg.Headroom))
	d := Decision{Tick: tick, Bandwidth: bw, TargetKbps: c.target}
	// achieved bitrate: bytes * 8 / slot duration in seconds
	slotMs := int64(1000 / c.clk.FPS)
	if deliveredBytes > 0 {
		d.ActualKbps = int(int64(deliveredBytes) * 8 * 1000 / slotMs / 1000)
	}
	if c.target > 0 {
		diff := d.ActualKbps - c.target
		if diff < 0 {
			diff = -diff
		}
		d.ErrorPctX10 = diff * 10000 / c.target
	}
	// adapt target for the next slot: back off on congestion, probe up
	// when there is clear headroom, always stay inside the bounds.
	switch {
	case c.target > afford:
		c.target = int(float64(c.target) * c.cfg.DecreaseFactor)
	case afford-c.target > c.cfg.IncreaseKbps*2:
		c.target += c.cfg.IncreaseKbps
	}
	if c.target < c.cfg.FloorKbps {
		c.target = c.cfg.FloorKbps
	}
	if c.target > c.cfg.CeilKbps {
		c.target = c.cfg.CeilKbps
	}
	return d, nil
}

// Summary aggregates a decision series into headline bitrate metrics.
type Summary struct {
	TargetKbps      int // configured starting target (headline metric)
	MeanTarget      int
	MeanActual      int
	MeanErrorPctX10 int
	LostFrames      int
	Decisions       []Decision
}

// Summarise builds a Summary from a completed decision series.
func Summarise(configuredTarget int, ds []Decision) Summary {
	s := Summary{TargetKbps: configuredTarget, Decisions: ds}
	var tSum, aSum, eSum int64
	for _, d := range ds {
		tSum += int64(d.TargetKbps)
		aSum += int64(d.ActualKbps)
		eSum += int64(d.ErrorPctX10)
		if d.ActualKbps == 0 {
			s.LostFrames++
		}
	}
	if n := int64(len(ds)); n > 0 {
		s.MeanTarget = int(tSum / n)
		s.MeanActual = int(aSum / n)
		s.MeanErrorPctX10 = int(eSum / n)
	}
	return s
}
