package pipeline

import (
	"sort"

	"avpipe/internal/clock"
	"avpipe/internal/fault"
)

// Options are the tunables exposed by the CLI.
type Options struct {
	Seed            uint64
	Loss            float64
	JitterMs        float64
	ConsumerDelayMs float64
	QueueBytes      int64
	QueueTicks      int64
	Policy          string
	SourceRate      int
	TargetRate      int
	SourceLayout    string
	TargetLayout    string
	TargetLUFS      float64
	PeakCeiling     float64
	DriftPPM        float64
	Frames          int
	BlockSize       int
	RecoveryWindow  int
	ReportPath      string
	WorkDir         string
	Input           string
	Layout          string
}

// Defaults fills unset fields with the documented defaults.
func (o *Options) Defaults() {
	if o.Seed == 0 {
		o.Seed = 1
	}
	if o.SourceRate == 0 {
		o.SourceRate = 48000
	}
	if o.TargetRate == 0 {
		o.TargetRate = 48000
	}
	if o.SourceLayout == "" {
		o.SourceLayout = "5.1"
	}
	if o.TargetLayout == "" {
		o.TargetLayout = "stereo"
	}
	if o.Layout == "" {
		o.Layout = o.TargetLayout
	}
	if o.TargetLUFS == 0 {
		o.TargetLUFS = -16
	}
	if o.PeakCeiling == 0 {
		o.PeakCeiling = 0.95
	}
	if o.QueueBytes == 0 {
		o.QueueBytes = 262144
	}
	if o.QueueTicks == 0 {
		o.QueueTicks = int64(clock.FromMillis(250))
	}
	if o.Policy == "" {
		o.Policy = "block"
	}
	if o.Frames == 0 {
		o.Frames = o.SourceRate // one second
	}
	if o.BlockSize == 0 {
		o.BlockSize = 1024
	}
	if o.RecoveryWindow == 0 {
		o.RecoveryWindow = 16
	}
	if o.DriftPPM == 0 {
		o.DriftPPM = 600
	}
	if o.Input == "" {
		o.Input = "generated"
	}
}

// Pacing models a real-time consumer. The consumer retires one block per
// BlockMs of media time, so it keeps pace with the source; ConsumerMs is the
// consumer's own playout latency added to every block. Transient arrival
// bursts are absorbed by the bounded queue instead of growing without limit.
type Pacing struct {
	BlockMs    float64
	ConsumerMs float64
}

// Simulate computes per-block residence latency. capacityBlocks is the queue
// depth derived from the byte and duration limits; policy decides what
// happens when an arrival finds the queue full.
func (p *Pacing) Simulate(capacityBlocks int, policy string, arrivals []float64) (latencies []float64, peak int, dropped int) {
	if len(arrivals) == 0 || p.BlockMs <= 0 {
		return nil, 0, 0
	}
	sort.Float64s(arrivals)
	queue := make([]float64, 0, len(arrivals))
	freeAt := 0.0
	service := func(arrival float64) {
		start := freeAt
		if start < arrival {
			start = arrival
		}
		freeAt = start + p.BlockMs
		latencies = append(latencies, freeAt-arrival+p.ConsumerMs)
	}
	for _, a := range arrivals {
		for len(queue) > 0 && freeAt <= a {
			service(queue[0])
			queue = queue[1:]
		}
		if capacityBlocks > 0 && len(queue) >= capacityBlocks {
			switch policy {
			case "drop":
				dropped++
				continue
			case "drop-oldest":
				dropped++
				queue = queue[1:]
			default: // block: the consumer must retire one block first
				service(queue[0])
				queue = queue[1:]
			}
		}
		queue = append(queue, a)
		if len(queue) > peak {
			peak = len(queue)
		}
	}
	for len(queue) > 0 {
		service(queue[0])
		queue = queue[1:]
	}
	return latencies, peak, dropped
}

// percentile returns the nearest-rank percentile.
func percentile(samples []float64, p float64) float64 {
	cp := append([]float64(nil), samples...)
	sort.Float64s(cp)
	return clock.Percentile(cp, p)
}

// classify maps an error to the CLI exit status.
func classify(err error) int { return fault.ExitCode(err) }

// capacityBlocks derives how many blocks fit under the tighter of the byte
// and duration limits.
func capacityBlocks(blockBytes int64, blockMs float64, maxBytes, maxTicks int64) int {
	byBytes := int(maxBytes / blockBytes)
	if byBytes < 1 {
		byBytes = 1
	}
	if maxTicks > 0 && blockMs > 0 {
		byTicks := int(float64(maxTicks) / (blockMs * float64(clock.Timescale) / 1000))
		if byTicks < 1 {
			byTicks = 1
		}
		if byTicks < byBytes {
			return byTicks
		}
	}
	return byBytes
}
