// Package engine wires clock repair, subtitle reconciliation, track-graph
// normalization, index building and quality statistics into one deterministic
// pipeline, and emits the machine-readable demo report.
package engine

import (
	"math"

	"avrepair/internal/clock"
	"avrepair/internal/fixture"
)

// Report is the stable JSON summary emitted per case. Field names match the
// required demo contract exactly.
type Report struct {
	Case              string  `json:"case"`
	TargetBPS         int64   `json:"target_bps"`
	ActualBPS         int64   `json:"actual_bps"`
	DriftPct          float64 `json:"drift_pct"`
	AudioRMSDeltaDB   float64 `json:"audio_rms_delta_db"`
	VideoLumaMAE      float64 `json:"video_luma_mae"`
	ManifestMaxSkewMS float64 `json:"manifest_max_skew_ms"`
	RepairCount       int     `json:"repair_count"`
	SeekMiss          int     `json:"seek_miss"`
	OutputSHA256      string  `json:"output_sha256"`
	// Diagnostics supplement the contract fields without changing them.
	Isolated     int                  `json:"isolated"`
	Errors       []*clock.TrackError `json:"errors,omitempty"`
	Indexed      int                  `json:"indexed"`
	RecoveredNS  int64                `json:"recovered_ns"`
	ExitCode     int                  `json:"exit_code"`
}

// bitrate computes target and actual bits-per-second over a verified interval.
// Both numerators count only packets whose timestamps fall inside the window,
// so truncated assets are measured against their real content duration.
func bitrate(spec fixture.Spec, video, audio []fixture.Packet, durationNS int64) (target, actual int64) {
	if durationNS <= 0 {
		return 0, 0
	}
	seconds := float64(durationNS) / float64(clock.Second)
	var targetBytes, actualBytes int64
	for _, p := range video {
		if p.TS < durationNS {
			targetBytes += int64(spec.VideoBytes)
			actualBytes += int64(p.Bytes)
		}
	}
	for _, p := range audio {
		if p.TS < durationNS {
			targetBytes += int64(spec.AudioBytes)
			actualBytes += int64(p.Bytes)
		}
	}
	target = int64(math.Round(float64(targetBytes*8) / seconds))
	actual = int64(math.Round(float64(actualBytes*8) / seconds))
	return target, actual
}

// rms returns the root-mean-square of PCM samples.
func rms(samples []int16) float64 {
	if len(samples) == 0 {
		return 0
	}
	var sum float64
	for _, s := range samples {
		v := float64(s)
		sum += v * v
	}
	return math.Sqrt(sum / float64(len(samples)))
}

// rmsDeltaDB compares the generated audio RMS against the analytic RMS of the
// reference sine (amplitude 20000). The deterministic fixture should land
// essentially on the reference, so the delta is expected at (or near) zero.
func audioRMSDeltaDB(packets []fixture.Packet) float64 {
	const referenceAmp = 20000.0
	referenceRMS := referenceAmp / math.Sqrt2
	var all []int16
	for _, p := range packets {
		all = append(all, p.Samples...)
	}
	got := rms(all)
	if got <= 0 {
		return 0
	}
	return 20 * math.Log10(got/referenceRMS)
}

// lumaMAE is the mean absolute error between observed packet luma and the
// ideal smooth ramp the fixture intends (ripple excluded from the ideal).
func lumaMAE(video []fixture.Packet, spec fixture.Spec) float64 {
	if len(video) == 0 {
		return 0
	}
	var sum float64
	n := 0
	for _, p := range video {
		ideal := 90.0 + 60.0*float64(p.Seq)/float64(spec.VideoFrames)
		sum += math.Abs(p.Luma - ideal)
		n++
	}
	return sum / float64(n)
}

// driftPercent returns the signed bitrate drift as a percentage of target.
func driftPercent(target, actual int64) float64 {
	if target == 0 {
		return 0
	}
	return 100 * float64(actual-target) / float64(target)
}
