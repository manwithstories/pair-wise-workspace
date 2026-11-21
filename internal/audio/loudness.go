package audio

import (
	"errors"
	"fmt"
	"math"

	"mediamerge/internal/fault"
)

// LoudnessTarget is the delivery loudness specification.
type LoudnessTarget struct {
	// TargetLUFS is integrated loudness. Zero means "do not normalize".
	TargetLUFS float64
	// TruePeakDBTP is the ceiling. Zero means "no ceiling".
	TruePeakDBTP float64
	// MaxGainDB bounds boost so that noise is not lifted arbitrarily.
	MaxGainDB float64
	// MinGainDB bounds cut so that a mostly silent bed is not destroyed.
	MinGainDB float64
}

// DefaultTarget is the demo delivery spec: -23 LUFS with a -1 dBTP ceiling.
func DefaultTarget() LoudnessTarget {
	return LoudnessTarget{TargetLUFS: -23, TruePeakDBTP: -1, MaxGainDB: 12, MinGainDB: -24}
}

// LoudnessStats is the deterministic measurement result and the applied gain.
type LoudnessStats struct {
	InputLUFS       float64
	InputPeak       float64 // linear
	TargetLUFS      float64
	PeakCeilingDBTP float64
	GainDB          float64
	AppliedGainDB   float64
	Limited         bool
	LimitReason     string
	OutputLUFS      float64
	OutputPeak      float64
	// Clipped counts samples that would have exceeded full scale.
	Clipped int
}

// measureLUFS computes mean-square loudness over all channels. This is an
// RMS-based approximation of integrated loudness; it is deterministic and
// independent of block boundaries, which is what the acceptance run needs.
// The absolute offset (-0.691 dB) is applied so that a full-scale sine
// measures close to its nominal level.
func measureLUFS(samples []int32) float64 {
	if len(samples) == 0 {
		return math.Inf(-1)
	}
	var sum float64
	for _, s := range samples {
		x := float64(s) / float64(1<<31)
		sum += x * x
	}
	ms := sum / float64(len(samples))
	if ms <= 0 {
		return math.Inf(-1)
	}
	return -0.691 + 10*math.Log10(ms)
}

func peakLinear(samples []int32) float64 {
	var max float64
	for _, s := range samples {
		v := math.Abs(float64(s) / float64(1<<31))
		if v > max {
			max = v
		}
	}
	return max
}

// LimitReason names why the requested gain was reduced.
const (
	LimitPeakCeiling = "PEAK_CEILING"
	LimitSilence     = "SILENCE"
	LimitGainBounds  = "GAIN_BOUNDS"
)

// ApplyLoudness measures samples, computes the gain that reaches the target,
// then limits it so the true peak stays under the ceiling. Every reduction is
// recorded with a machine-readable reason. The statistics describe the actual
// applied gain, not the requested one.
func ApplyLoudness(trackID int, samples []int32, target LoudnessTarget) ([]int32, LoudnessStats, error) {
	stats := LoudnessStats{
		TargetLUFS:      target.TargetLUFS,
		PeakCeilingDBTP: target.TruePeakDBTP,
	}
	if len(samples) == 0 {
		return nil, stats, fault.NewTrack(fault.StageAudio, fault.CodeInvalidArgument, trackID, "empty audio buffer")
	}
	stats.InputLUFS = measureLUFS(samples)
	stats.InputPeak = peakLinear(samples)

	out := make([]int32, len(samples))
	if target.TargetLUFS == 0 {
		copy(out, samples)
		stats.OutputLUFS = stats.InputLUFS
		stats.OutputPeak = stats.InputPeak
		return out, stats, nil
	}
	if math.IsInf(stats.InputLUFS, -1) {
		copy(out, samples)
		stats.Limited = true
		stats.LimitReason = LimitSilence
		stats.OutputLUFS = stats.InputLUFS
		stats.OutputPeak = stats.InputPeak
		return out, stats, fault.NewTrack(fault.StageAudio, fault.CodeLoudnessTarget, trackID,
			"silent input has no measurable loudness; target cannot be reached")
	}

	stats.GainDB = target.TargetLUFS - stats.InputLUFS
	if stats.GainDB > target.MaxGainDB {
		stats.GainDB = target.MaxGainDB
		stats.Limited = true
		stats.LimitReason = LimitGainBounds
	}
	if stats.GainDB < target.MinGainDB {
		stats.GainDB = target.MinGainDB
		stats.Limited = true
		stats.LimitReason = LimitGainBounds
	}

	// Peak ceiling: gain + peak_dB <= ceiling.
	if target.TruePeakDBTP != 0 && stats.InputPeak > 0 {
		peakDB := 20 * math.Log10(stats.InputPeak)
		maxGain := target.TruePeakDBTP - peakDB
		// Keep a small safety margin so int32 rounding cannot push a
		// sample over the ceiling.
		maxGain -= 0.05
		if maxGain < stats.GainDB {
			stats.GainDB = maxGain
			stats.Limited = true
			stats.LimitReason = LimitPeakCeiling
		}
	}

	stats.AppliedGainDB = stats.GainDB
	gain := math.Pow(10, stats.GainDB/20)
	var clipped int
	for i, s := range samples {
		v := math.Round(float64(s) * gain)
		if v > math.MaxInt32 || v < math.MinInt32 {
			clipped++
			if v > 0 {
				v = math.MaxInt32
			} else {
				v = math.MinInt32
			}
		}
		out[i] = int32(v)
	}
	stats.Clipped = clipped
	if clipped > 0 {
		stats.Limited = true
		if stats.LimitReason == "" {
			stats.LimitReason = LimitPeakCeiling
		}
	}
	stats.OutputLUFS = measureLUFS(out)
	stats.OutputPeak = peakLinear(out)
	return out, stats, nil
}

// Measure returns integrated loudness and linear peak for a buffer. It is
// exported so a streaming producer can accumulate statistics window by window
// without retaining the audio.
func Measure(samples []int32) (lufs, peak float64) {
	return measureLUFS(samples), peakLinear(samples)
}

// Accumulator sums loudness energy across windows so the pipeline can measure
// a full track while holding one window at a time.
type Accumulator struct {
	sumSquares float64
	count      int64
	peak       float64
}

// Add folds one window into the accumulator.
func (a *Accumulator) Add(samples []int32) {
	for _, s := range samples {
		x := float64(s) / float64(1<<31)
		a.sumSquares += x * x
		if v := math.Abs(x); v > a.peak {
			a.peak = v
		}
	}
	a.count += int64(len(samples))
}

// LUFS returns the integrated loudness seen so far.
func (a *Accumulator) LUFS() float64 {
	if a.count == 0 || a.sumSquares <= 0 {
		return math.Inf(-1)
	}
	return -0.691 + 10*math.Log10(a.sumSquares/float64(a.count))
}

// Peak returns the linear peak seen so far.
func (a *Accumulator) Peak() float64 { return a.peak }

// Count returns the number of samples folded in.
func (a *Accumulator) Count() int64 { return a.count }

// PlanGain computes the gain and its limiting decisions from pre-measured
// input loudness and peak. It never touches sample data, so the caller can
// measure in one streaming pass and apply in a second.
func PlanGain(trackID int, inputLUFS, inputPeak float64, target LoudnessTarget) (LoudnessStats, error) {
	stats := LoudnessStats{
		TargetLUFS:      target.TargetLUFS,
		PeakCeilingDBTP: target.TruePeakDBTP,
		InputLUFS:       inputLUFS,
		InputPeak:       inputPeak,
	}
	if target.TargetLUFS == 0 {
		stats.OutputLUFS = inputLUFS
		stats.OutputPeak = inputPeak
		return stats, nil
	}
	if math.IsInf(inputLUFS, -1) {
		stats.Limited = true
		stats.LimitReason = LimitSilence
		stats.OutputLUFS = inputLUFS
		stats.OutputPeak = inputPeak
		return stats, fault.NewTrack(fault.StageAudio, fault.CodeLoudnessTarget, trackID,
			"silent input has no measurable loudness; target cannot be reached")
	}
	stats.GainDB = target.TargetLUFS - inputLUFS
	if stats.GainDB > target.MaxGainDB {
		stats.GainDB = target.MaxGainDB
		stats.Limited = true
		stats.LimitReason = LimitGainBounds
	}
	if stats.GainDB < target.MinGainDB {
		stats.GainDB = target.MinGainDB
		stats.Limited = true
		stats.LimitReason = LimitGainBounds
	}
	if target.TruePeakDBTP != 0 && inputPeak > 0 {
		peakDB := 20 * math.Log10(inputPeak)
		maxGain := target.TruePeakDBTP - peakDB - 0.05
		if maxGain < stats.GainDB {
			stats.GainDB = maxGain
			stats.Limited = true
			stats.LimitReason = LimitPeakCeiling
		}
	}
	stats.AppliedGainDB = stats.GainDB
	return stats, nil
}

// ApplyGain scales samples by gainDB and clamps to int32. It returns the
// number of samples that hit the clamp.
func ApplyGain(samples []int32, gainDB float64) ([]int32, int) {
	gain := math.Pow(10, gainDB/20)
	out := make([]int32, len(samples))
	var clipped int
	for i, s := range samples {
		v := math.Round(float64(s) * gain)
		if v > math.MaxInt32 || v < math.MinInt32 {
			clipped++
			if v > 0 {
				v = math.MaxInt32
			} else {
				v = math.MinInt32
			}
		}
		out[i] = int32(v)
	}
	return out, clipped
}

// StatsValid reports whether the applied gain stayed inside the target
// bounds. It is used by the stage validator.
func (s LoudnessStats) StatsValid() error {
	if s.Limited && s.LimitReason == "" {
		return errors.New("audio: loudness was limited without a recorded reason")
	}
	if s.Clipped > 0 {
		return fmt.Errorf("audio: %d samples clipped despite the peak ceiling", s.Clipped)
	}
	if s.TargetLUFS != 0 && math.Abs(s.OutputLUFS-s.TargetLUFS) > 3 {
		return fmt.Errorf("audio: output %.2f LUFS is more than 3 LU from target %.2f", s.OutputLUFS, s.TargetLUFS)
	}
	return nil
}
