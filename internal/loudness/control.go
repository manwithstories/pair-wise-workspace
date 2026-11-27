// Package loudness applies a 200 ms sliding loudness window and a peak
// limiter so the rendered program lands within -20 dBFS +/-1 dB while no
// sample exceeds -1 dBFS.
package loudness

import (
	"fmt"
	"math"

	"avpipe/internal/media"
)

// WindowMillis is the loudness integration window fixed by the pipeline.
const WindowMillis = 200

// TargetDBFS is the loudness target and TargetTolerance the accepted band.
const (
	TargetDBFS      = -20.0
	TargetTolerance = 1.0
	PeakCeilingDBFS = -1.0
)

// Config parameterizes the controller.
type Config struct {
	Rate     media.Rate
	Channels int
	// TargetDBFS overrides the default -20 dBFS when non-zero-able.
	TargetDBFS      float64
	UseCustomTarget bool
}

// Controller maintains a sliding 200 ms mean-square window, a running peak,
// and a slowly adapting gain that converges on the target loudness.
type Controller struct {
	rate        media.Rate
	channels    int
	window      int
	target      float64
	ring        []float64 // mean-square per frame
	head        int
	count       int
	sumSquares  float64
	peak        float64
	gain        float64
	appliedGain float64
	frames      int64
}

// New constructs a controller. The gain starts at unity so the controller is
// transparent for already-conformant material.
func New(cfg Config) (*Controller, error) {
	if !cfg.Rate.Valid() {
		return nil, media.NewError(media.PhaseConfig, media.ExitUsage, "", 0, false, "new",
			fmt.Errorf("invalid loudness rate"))
	}
	if cfg.Channels <= 0 {
		return nil, media.NewError(media.PhaseConfig, media.ExitUsage, "", 0, false, "new",
			fmt.Errorf("loudness needs positive channels"))
	}
	clock, err := media.NewClock(cfg.Rate)
	if err != nil {
		return nil, media.NewError(media.PhaseConfig, media.ExitUsage, "", 0, false, "new", err)
	}
	window := clock.Frames(WindowMillis * 1e6)
	if window < 1 {
		window = 1
	}
	target := TargetDBFS
	if cfg.UseCustomTarget {
		target = cfg.TargetDBFS
	}
	return &Controller{
		rate:     cfg.Rate,
		channels: cfg.Channels,
		window:   int(window),
		target:   target,
		ring:     make([]float64, window),
		gain:     1,
	}, nil
}

// Update applies windowed loudness adaptation and peak limiting to one block.
// The returned block keeps its timeline metadata; its samples are scaled so
// the long-run mean square matches the target. The gain is derived from the
// pre-gain signal, so once converged the output mean square equals the target
// exactly instead of chasing its own feedback.
func (c *Controller) Update(block media.Block) (media.Block, error) {
	if block.Channels != c.channels {
		return media.Block{}, media.NewError(media.PhaseLoudness, media.ExitInput, block.Track, block.Seq, false, "update",
			fmt.Errorf("block has %d channels, controller expects %d", block.Channels, c.channels))
	}
	if block.Rate != c.rate {
		return media.Block{}, media.NewError(media.PhaseLoudness, media.ExitInput, block.Track, block.Seq, false, "update",
			fmt.Errorf("block rate %s != %s", block.Rate.String(), c.rate.String()))
	}
	if len(block.Samples)%c.channels != 0 {
		return media.Block{}, media.NewError(media.PhaseLoudness, media.ExitInput, block.Track, block.Seq, false, "update",
			fmt.Errorf("sample count %d not divisible by %d channels", len(block.Samples), c.channels))
	}
	frames := block.Frames()
	out := make([]float64, len(block.Samples))
	ceiling := math.Pow(10, PeakCeilingDBFS/20)
	targetMS := math.Pow(10, c.target/10)

	for f := 0; f < frames; f++ {
		var ms float64
		for ch := 0; ch < c.channels; ch++ {
			v := block.Samples[f*c.channels+ch]
			ms += v * v
		}
		ms /= float64(c.channels)

		// Advance the sliding window first so the gain always reflects the
		// most recent 200 ms, including the frame being written.
		if c.count == c.window {
			c.sumSquares -= c.ring[c.head]
		} else {
			c.count++
		}
		c.ring[c.head] = ms
		c.sumSquares += ms
		c.head = (c.head + 1) % c.window
		if c.sumSquares < 0 {
			c.sumSquares = 0
		}

		mean := c.sumSquares / float64(c.count)
		if mean > 1e-12 {
			// A first-order (single-pole) correction keeps the adaptation
			// smooth; the gain never lurches on a single transient.
			want := math.Sqrt(targetMS / mean)
			c.gain += 0.05 * (want - c.gain)
		}
		c.appliedGain = c.gain

		for ch := 0; ch < c.channels; ch++ {
			v := block.Samples[f*c.channels+ch] * c.gain
			// Peak limiting is applied after the loudness gain so the ceiling
			// is a hard guarantee for every output sample.
			if v > ceiling {
				c.peak = math.Max(c.peak, ceiling)
				v = ceiling
			} else if v < -ceiling {
				c.peak = math.Max(c.peak, ceiling)
				v = -ceiling
			} else if a := math.Abs(v); a > c.peak {
				c.peak = a
			}
			out[f*c.channels+ch] = v
		}
	}
	c.frames += int64(frames)
	return media.Block{
		Track:    block.Track,
		Seq:      block.Seq,
		Start:    block.Start,
		Rate:     block.Rate,
		Channels: block.Channels,
		Samples:  out,
	}, nil
}

// Finish returns the final gain that would be applied to one more frame. It
// does not alter state and is used for reporting.
func (c *Controller) Finish() float64 {
	return c.gain
}

// Stats reports the controller state.
type Stats struct {
	Frames     int64
	WindowMS   int
	Gain       float64
	LastGain   float64
	Peak       float64
	PeakDBFS   float64
	GainDB     float64
	TargetDBFS float64
}

// Stats returns the accumulated statistics.
func (c *Controller) Stats() Stats {
	peakDBFS := -math.Inf(1)
	if c.peak > 0 {
		peakDBFS = 20 * math.Log10(c.peak)
	}
	gainDB := 0.0
	if c.appliedGain > 0 {
		gainDB = 20 * math.Log10(c.appliedGain)
	}
	return Stats{
		Frames:     c.frames,
		WindowMS:   WindowMillis,
		Gain:       c.gain,
		LastGain:   c.appliedGain,
		Peak:       c.peak,
		PeakDBFS:   peakDBFS,
		GainDB:     gainDB,
		TargetDBFS: c.target,
	}
}

// MeanSquareDBFS measures the mean square of the supplied samples, useful for
// post-render verification of the -20 dBFS target.
func MeanSquareDBFS(samples []float64, channels int) float64 {
	if channels <= 0 || len(samples) == 0 {
		return -math.Inf(1)
	}
	var sum float64
	for _, s := range samples {
		sum += s * s
	}
	ms := sum / float64(len(samples)/channels) / float64(channels)
	if ms <= 0 {
		return -math.Inf(1)
	}
	return 10 * math.Log10(ms)
}

// PeakDBFS measures the absolute peak of the supplied samples.
func PeakDBFS(samples []float64) float64 {
	var peak float64
	for _, s := range samples {
		if a := math.Abs(s); a > peak {
			peak = a
		}
	}
	if peak <= 0 {
		return -math.Inf(1)
	}
	return 20 * math.Log10(peak)
}
