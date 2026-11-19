package audio

import (
	"math"

	"avpipe/internal/fault"
)

// LoudnessStats summarizes RMS, peak and gain before and after loudness
// control. It never contains raw samples.
type LoudnessStats struct {
	RMSBefore   float64 `json:"rms_before"`
	RMSAfter    float64 `json:"rms_after"`
	PeakBefore  float64 `json:"peak_before"`
	PeakAfter   float64 `json:"peak_after"`
	GainDB      float64 `json:"gain_db"`
	LUFSBefore  float64 `json:"lufs_before"`
	LUFSAfter   float64 `json:"lufs_after"`
	PeakLimited bool    `json:"peak_limited"`
	Silent      bool    `json:"silent"`
}

// LoudnessConfig describes target loudness and peak ceiling.
type LoudnessConfig struct {
	TargetLUFS  float64
	PeakCeiling float64
}

// measure returns RMS and absolute peak of interleaved samples.
func measure(samples []float64) (rms, peak float64) {
	if len(samples) == 0 {
		return 0, 0
	}
	var sum float64
	for _, v := range samples {
		sum += v * v
		if a := math.Abs(v); a > peak {
			peak = a
		}
	}
	return math.Sqrt(sum / float64(len(samples))), peak
}

// measureBlock measures one block and returns per-block values.
func measureBlock(b Block) (rms, peak float64) { return measure(b.Data) }

// rmsToLUFS approximates loudness from RMS. Full-scale sine RMS is 1/sqrt(2),
// taken as 0 LUFS for this diagnostic pipeline; the value is only used to
// compare before/after and to drive the gain.
func rmsToLUFS(rms float64) float64 {
	if rms <= 0 {
		return -math.Inf(1)
	}
	return 20 * math.Log10(rms*math.Sqrt2)
}

// lufsToGain returns the linear gain that moves rms to target LUFS.
func lufsToGain(rms, targetLUFS float64) float64 {
	if rms <= 0 {
		return 1
	}
	want := math.Pow(10, targetLUFS/20) / math.Sqrt2
	g := want / rms
	if math.IsInf(g, 0) || math.IsNaN(g) {
		return 1
	}
	return g
}

// Loudness applies a single deterministic gain to a whole drained stream.
// Silent blocks are never amplified: their gain is forced to exactly 1 so
// digital silence cannot be turned into noise. Peak limiting clamps the
// post-gain samples to the ceiling and reports whether it engaged.
func Loudness(blocks []Block, cfg LoudnessConfig) ([]Block, LoudnessStats) {
	var (
		all     []float64
		peakPre float64
		silent  = true
	)
	for _, b := range blocks {
		all = append(all, b.Data...)
		_, p := measure(b.Data)
		if p > peakPre {
			peakPre = p
		}
		if p > 0 {
			silent = false
		}
	}
	if cfg.PeakCeiling <= 0 || cfg.PeakCeiling > 1 {
		cfg.PeakCeiling = 0.98
	}
	rmsPre, _ := measure(all)
	stats := LoudnessStats{
		RMSBefore: rmsPre, PeakBefore: peakPre,
		LUFSBefore: rmsToLUFS(rmsPre), Silent: silent,
	}
	gain := 1.0
	if !silent {
		gain = lufsToGain(rmsPre, cfg.TargetLUFS)
		// Do not let the gain push the peak above the ceiling; the limiter
		// below would do it anyway, but pre-scaling avoids unnecessary
		// clipping of the loudest transients.
		if peakPre*gain > cfg.PeakCeiling {
			gain = cfg.PeakCeiling / peakPre
			stats.PeakLimited = true
		}
	}
	out := make([]Block, 0, len(blocks))
	var peakPost float64
	for _, b := range blocks {
		nb := b
		nb.Data = make([]float64, len(b.Data))
		_, p := measureBlock(b)
		blockGain := gain
		if p == 0 {
			blockGain = 1 // silence is never amplified
		}
		for i, v := range b.Data {
			v *= blockGain
			if v > cfg.PeakCeiling {
				v, stats.PeakLimited = cfg.PeakCeiling, true
			} else if v < -cfg.PeakCeiling {
				v, stats.PeakLimited = -cfg.PeakCeiling, true
			}
			if a := math.Abs(v); a > peakPost {
				peakPost = a
			}
			nb.Data[i] = v
		}
		out = append(out, nb)
	}
	var post []float64
	for _, b := range out {
		post = append(post, b.Data...)
	}
	rmsPost, _ := measure(post)
	stats.RMSAfter, stats.PeakAfter = rmsPost, peakPost
	stats.LUFSAfter = rmsToLUFS(rmsPost)
	if gain > 0 {
		stats.GainDB = 20 * math.Log10(gain)
	}
	stats.Silent = silent
	return out, stats
}

// ValidateLoudnessConfig rejects impossible targets early.
func ValidateLoudnessConfig(cfg LoudnessConfig) error {
	if cfg.TargetLUFS < -70 || cfg.TargetLUFS > 0 {
		return fault.New(fault.CodeInvalidArgument, "audio.loudness",
			"target %0.1f LUFS outside -70..0", cfg.TargetLUFS)
	}
	if cfg.PeakCeiling != 0 && (cfg.PeakCeiling <= 0 || cfg.PeakCeiling > 1) {
		return fault.New(fault.CodeInvalidArgument, "audio.loudness",
			"peak ceiling %0.2f must be in (0,1]", cfg.PeakCeiling)
	}
	return nil
}
