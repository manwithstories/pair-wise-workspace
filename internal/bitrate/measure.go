// Package bitrate computes target/actual bitrate deviation and the
// quality summary over a recovered stream.
package bitrate

import (
	"math"

	"mediarchive/internal/clock"
)

// Counter aggregates the measurements of one archive run.
type Counter struct {
	PacketsIn    int
	Duplicates   int
	Lost         int // packets inside loss gaps
	LossBursts   int
	LateReplays  int
	TooLate      int
	Quarantined  int
	RepairedTS   int
	PayloadBytes int64
	MediaTime    clock.Time // sum of accepted packet durations
	TargetKbps   float64
}

// ActualKbps is the measured media bitrate of the recovered stream.
func (c Counter) ActualKbps() float64 {
	sec := c.MediaTime.Duration().Seconds()
	if sec <= 0 {
		return 0
	}
	return float64(c.PayloadBytes) * 8 / sec / 1000
}

// ErrorPct is the absolute deviation from the target bitrate in percent.
func (c Counter) ErrorPct() float64 {
	if c.TargetKbps <= 0 {
		return 0
	}
	return math.Abs(c.ActualKbps()-c.TargetKbps) / c.TargetKbps * 100
}

// QualityScore condenses the run into a 0-100 score. The formula is
// deterministic: every penalty derives from the counters above.
func (c Counter) QualityScore() float64 {
	if c.PacketsIn <= 0 {
		return 0
	}
	in := float64(c.PacketsIn)
	score := 100.0
	score -= float64(c.Lost+c.Quarantined) / in * 100 * 2
	score -= float64(c.Duplicates) / in * 100 * 0.5
	score -= float64(c.TooLate) / in * 100 * 2
	score -= c.ErrorPct()
	if score < 0 {
		score = 0
	}
	return math.Round(score*100) / 100
}
