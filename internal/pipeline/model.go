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

// percentile returns the nearest-rank percentile.
func percentile(samples []float64, p float64) float64 {
	cp := append([]float64(nil), samples...)
	sort.Float64s(cp)
	return clock.Percentile(cp, p)
}

// classify maps an error to the CLI exit status.
func classify(err error) int { return fault.ExitCode(err) }
