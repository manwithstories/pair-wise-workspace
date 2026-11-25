// Package audio detects silence in the private PCM16LE track using windowed
// RMS energy with a threshold and entry/exit hysteresis, so isolated samples
// cannot flap the state. It also scores detection against known ground truth.
package audio

import (
	"math"

	"streamcut/internal/media"
)

// Interval is a detected or reference silence range in 90 kHz ticks.
type Interval struct {
	Start media.Time90
	End   media.Time90
}

// Config parameterizes the detector.
type Config struct {
	SampleRate      int
	WindowSamples   int     // analysis window, 960 == 20 ms at 48 kHz
	ThresholdRMS    float64 // amplitude RMS under which a window is quiet
	EnterWindows    int     // consecutive quiet windows to enter silence
	ExitWindows     int     // consecutive loud windows to leave silence
}

// DefaultConfig is the 48 kHz / 20 ms configuration matching the fixtures:
// silent dither sits at <=1 LSB, tones run ~8500 RMS, leaving a wide margin.
func DefaultConfig() Config {
	return Config{
		SampleRate:    48000,
		WindowSamples: 960,
		ThresholdRMS: 256,
		EnterWindows:  2,
		ExitWindows:   2,
	}
}

// Detector is a streaming hysteresis silence detector.
type Detector struct {
	cfg      Config
	pending  []int16 // samples held until a full window accumulates
	quietRun int
	loudRun  int
	inSilence bool
	windowIdx int
	silenceStart media.Time90
	out      []Interval
}

// NewDetector constructs a detector.
func NewDetector(cfg Config) *Detector {
	return &Detector{cfg: cfg}
}

// PushPCM appends little-endian PCM16 samples stamped at pts. Windows are
// fixed-size in sample count; the PTS advances at SampleRate.
func (d *Detector) PushPCM(pcm []byte, pts media.Time90) {
	for i := 0; i+1 < len(pcm); i += 2 {
		s := int16(pcm[i]) | int16(pcm[i+1])<<8
		d.pending = append(d.pending, s)
		if len(d.pending) == d.cfg.WindowSamples {
			win := d.pending
			d.pending = nil
			d.evaluate(win, pts)
		}
	}
}

func (d *Detector) evaluate(win []int16, pts media.Time90) {
	var sum float64
	for _, s := range win {
		sum += float64(s) * float64(s)
	}
	rms := math.Sqrt(sum / float64(len(win)))
	tickPerWindow := media.Time90(d.cfg.WindowSamples) *
		media.TicksPerSecond / media.Time90(d.cfg.SampleRate)
	winStart := pts - tickPerWindow

	if rms < d.cfg.ThresholdRMS {
		d.quietRun++
		d.loudRun = 0
	} else {
		d.loudRun++
		d.quietRun = 0
	}

	if !d.inSilence && d.quietRun >= d.cfg.EnterWindows {
		d.inSilence = true
		// Backdate the start to the first window of the quiet run.
		d.silenceStart = winStart -
			media.Time90(d.quietRun-1)*tickPerWindow
	}
	if d.inSilence && d.loudRun >= d.cfg.ExitWindows {
		d.inSilence = false
		end := winStart -
			media.Time90(d.loudRun-1)*tickPerWindow
		d.out = append(d.out, Interval{Start: d.silenceStart, End: end})
	}
	d.windowIdx++
}

// Flush closes any open silence interval at end-of-stream and returns all
// detected intervals in chronological order.
func (d *Detector) Flush() []Interval {
	out := append([]Interval(nil), d.out...)
	if d.inSilence {
		tickPerWindow := media.Time90(d.cfg.WindowSamples) *
			media.TicksPerSecond / media.Time90(d.cfg.SampleRate)
		end := media.Time90(d.windowIdx) * tickPerWindow
		out = append(out, Interval{Start: d.silenceStart, End: end})
	}
	return out
}

// Scores are frame-level precision/recall against ground truth.
type Scores struct {
	TruePositive  int
	FalsePositive int
	FalseNegative int
	Precision     float64
	Recall        float64
	F1            float64
}

// Score compares detected intervals with reference intervals at the given
// tick granularity, treating each tick bucket as one binary classification.
func Score(detected, reference []Interval, granularity media.Time90) Scores {
	ref := bucketize(reference, granularity)
	got := bucketize(detected, granularity)
	var s Scores
	for k := range unionKeys(ref, got) {
		r, g := ref[k], got[k]
		switch {
		case r && g:
			s.TruePositive++
		case !r && g:
			s.FalsePositive++
		case r && !g:
			s.FalseNegative++
		}
	}
	if s.TruePositive+s.FalsePositive > 0 {
		s.Precision = float64(s.TruePositive) /
			float64(s.TruePositive+s.FalsePositive)
	}
	if s.TruePositive+s.FalseNegative > 0 {
		s.Recall = float64(s.TruePositive) /
			float64(s.TruePositive+s.FalseNegative)
	}
	if s.Precision+s.Recall > 0 {
		s.F1 = 2 * s.Precision * s.Recall / (s.Precision + s.Recall)
	}
	return s
}

func bucketize(iv []Interval, g media.Time90) map[int64]bool {
	m := map[int64]bool{}
	for _, in := range iv {
		for t := in.Start; t < in.End; t += g {
			m[int64(t/g)] = true
		}
	}
	return m
}

func unionKeys(a, b map[int64]bool) map[int64]struct{} {
	out := make(map[int64]struct{}, len(a)+len(b))
	for k := range a {
		out[k] = struct{}{}
	}
	for k := range b {
		out[k] = struct{}{}
	}
	return out
}
