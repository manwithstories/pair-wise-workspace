package quality

import (
	"math"

	"mediaflow/internal/clock"
	"mediaflow/internal/timeline"
)

// SilenceConfig tunes RMS-based silence detection with hysteresis.
type SilenceConfig struct {
	// EnterThreshold is the RMS level below which silence starts.
	EnterThreshold float64
	// ExitThreshold is the RMS level above which audio resumes. It must exceed
	// EnterThreshold to provide hysteresis and avoid chattering.
	ExitThreshold float64
	// MinDurationUS ignores shorter isolated dips.
	MinDurationUS int64
}

// DefaultSilenceConfig uses -50 dBFS enter / -42 dBFS exit hysteresis.
func DefaultSilenceConfig() SilenceConfig {
	return SilenceConfig{
		EnterThreshold: math.Pow(10, -50.0/20.0),
		ExitThreshold:  math.Pow(10, -42.0/20.0),
		MinDurationUS:  100_000,
	}
}

// SilenceRange is a merged half-open silent interval on the timeline.
type SilenceRange struct {
	StartUS int64
	EndUS   int64
	MinRMS  float64
	MaxRMS  float64
}

// AudioFrame pairs a normalized audio frame with its PCM samples in [-1,1].
type AudioFrame struct {
	Frame   clock.Normalized
	Samples []float64
}

// DetectSilence computes per-frame RMS and applies enter/exit hysteresis, then
// merges adjacent silent frames and drops ranges shorter than the minimum.
func DetectSilence(frames []AudioFrame, cfg SilenceConfig) ([]SilenceRange, error) {
	if cfg.ExitThreshold <= cfg.EnterThreshold {
		return nil, timeline.GlobalError("quality.silence", false,
			timeline.Wrap("quality.silence", 0, 0, timeline.RationalZero, 0, false,
				errSilenceHysteresis(cfg)))
	}
	var ranges []SilenceRange
	var cur *SilenceRange
	for _, af := range frames {
		rms := RMS(af.Samples)
		silent := rms < cfg.EnterThreshold
		audible := rms >= cfg.ExitThreshold

		switch {
		case cur == nil && silent:
			cur = &SilenceRange{StartUS: af.Frame.StartUS, EndUS: af.Frame.StartUS + af.Frame.Duration,
				MinRMS: rms, MaxRMS: rms}
		case cur != nil:
			if audible {
				ranges = append(ranges, *cur)
				cur = nil
			} else {
				// Between enter and exit thresholds: hysteresis keeps the state.
				cur.EndUS = af.Frame.StartUS + af.Frame.Duration
				if rms < cur.MinRMS {
					cur.MinRMS = rms
				}
				if rms > cur.MaxRMS {
					cur.MaxRMS = rms
				}
			}
		}
	}
	if cur != nil {
		ranges = append(ranges, *cur)
	}

	merged := make([]SilenceRange, 0, len(ranges))
	for _, r := range ranges {
		if r.EndUS-r.StartUS < cfg.MinDurationUS {
			continue
		}
		if len(merged) > 0 && merged[len(merged)-1].EndUS == r.StartUS {
			merged[len(merged)-1].EndUS = r.EndUS
			continue
		}
		merged = append(merged, r)
	}
	return merged, nil
}

// RMS returns the root-mean-square amplitude of PCM samples.
func RMS(samples []float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	var sumSq float64
	for _, s := range samples {
		sumSq += s * s
	}
	return math.Sqrt(sumSq / float64(len(samples)))
}

func errSilenceHysteresis(cfg SilenceConfig) error {
	return &timeline.Error{
		Op: "quality.silence", Recoverable: false,
		Err: &hysteresisError{enter: cfg.EnterThreshold, exit: cfg.ExitThreshold},
	}
}

type hysteresisError struct{ enter, exit float64 }

func (e *hysteresisError) Error() string {
	return "silence exit threshold must exceed enter threshold"
}
