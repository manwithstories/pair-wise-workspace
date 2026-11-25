// Package audio decodes media tracks to PCM through the FFmpeg command
// line and performs windowed silence detection. All analysis runs on
// fixed-size windows streamed from the decoder, so memory use is bounded
// regardless of clip duration.
package audio

import (
	"math"

	"medpipe/internal/clock"
)

// SilenceConfig parameterizes the RMS detector.
type SilenceConfig struct {
	SampleRate       int     // PCM sample rate after resampling
	WindowMillis     int     // analysis window length, e.g. 20
	ThresholdRMS     float64 // linear RMS amplitude below which a window is silent
	MinSilenceMillis int     // minimum contiguous silence to report a range
}

// DefaultSilenceConfig targets 20 ms windows, -50 dBFS and 300 ms runs.
func DefaultSilenceConfig(rate int) SilenceConfig {
	return SilenceConfig{
		SampleRate:       rate,
		WindowMillis:     20,
		ThresholdRMS:     math.Pow(10, -50.0/20.0),
		MinSilenceMillis: 300,
	}
}

// Range is one detected silence span in PCM sample positions and millis.
type Range struct {
	StartSample int
	EndSample   int
	StartMillis int
	EndMillis   int
}

// Millis returns the silence duration as a non-negative integer.
func (r Range) Millis() int {
	if r.EndMillis < r.StartMillis {
		return 0
	}
	return r.EndMillis - r.StartMillis
}

// Detector accumulates PCM blocks, slices them into fixed windows and tracks
// contiguous silent runs. A trailing partial window never triggers silence by
// itself, so short boundary frames cannot produce false positives.
type Detector struct {
	cfg        SilenceConfig
	windowSize int
	minWindows int
	buf        []int16 // samples carried over to complete the next window
	samplePos  int     // absolute PCM sample index
	runStart   int     // start sample of the current silent run; -1 if none
	ranges     []Range
}

// NewDetector builds a detector and rejects unusable configuration.
func NewDetector(cfg SilenceConfig) (*Detector, error) {
	if cfg.SampleRate <= 0 || cfg.WindowMillis <= 0 || cfg.MinSilenceMillis < 0 {
		return nil, clock.E(clock.KindArgument, "audio.silence",
			clock.WithDetail("invalid silence config: %+v", cfg))
	}
	win := cfg.SampleRate * cfg.WindowMillis / 1000
	if win <= 0 {
		return nil, clock.E(clock.KindArgument, "audio.silence",
			clock.WithDetail("window rounds to zero samples at rate %d", cfg.SampleRate))
	}
	return &Detector{
		cfg:        cfg,
		windowSize: win,
		minWindows: (cfg.MinSilenceMillis + cfg.WindowMillis - 1) / cfg.WindowMillis,
		runStart:   -1,
	}, nil
}

// Push feeds a decoded PCM block (mono, s16le) into the detector.
func (d *Detector) Push(block []int16) {
	d.buf = append(d.buf, block...)
	for len(d.buf) >= d.windowSize {
		win := d.buf[:d.windowSize]
		d.buf = d.buf[d.windowSize:]
		silent := rms(win) < d.cfg.ThresholdRMS
		d.consumeWindow(silent)
	}
}

// Flush closes detection. Any leftover samples form a partial window that is
// excluded from silence accounting; an active run is finalized at the last
// full window boundary.
func (d *Detector) Flush() []Range {
	if d.runStart >= 0 {
		d.closeRun(d.samplePos)
	}
	d.buf = d.buf[:0]
	d.runStart = -1
	return d.ranges
}

func (d *Detector) consumeWindow(silent bool) {
	start := d.samplePos
	d.samplePos += d.windowSize
	if !silent {
		if d.runStart >= 0 {
			d.closeRun(start) // run ended at the previous full-window boundary
		}
		return
	}
	if d.runStart < 0 {
		d.runStart = start
	}
}

func (d *Detector) closeRun(endSample int) {
	windows := (endSample - d.runStart) / d.windowSize
	if windows >= d.minWindows {
		d.ranges = append(d.ranges, Range{
			StartSample: d.runStart,
			EndSample:   endSample,
			StartMillis: d.toMillis(d.runStart),
			EndMillis:   d.toMillis(endSample),
		})
	}
	d.runStart = -1
}

func (d *Detector) toMillis(samples int) int {
	return int((int64(samples)*1000 + int64(d.cfg.SampleRate)/2) / int64(d.cfg.SampleRate))
}

// TotalSilenceMillis sums every reported range as a non-negative integer.
func (d *Detector) TotalSilenceMillis() int {
	total := 0
	for _, r := range d.ranges {
		total += r.Millis()
	}
	return total
}

// rms computes the root mean square amplitude of one full window.
func rms(win []int16) float64 {
	var sum float64
	for _, s := range win {
		v := float64(s) / 32768.0
		sum += v * v
	}
	return math.Sqrt(sum / float64(len(win)))
}
