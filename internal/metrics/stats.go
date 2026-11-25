// Package metrics computes deterministic bitrate, loudness-facing and queue
// statistics used by terminal reports and manifests.
package metrics

import (
	"fmt"

	"mediacheck/internal/media"
	"mediacheck/internal/mux"
)

// BitrateTolerancePercent is the accepted target/actual deviation.
const BitrateTolerancePercent = 5.0

// SegmentReport is per-keyframe-segment bitrate accounting.
type SegmentReport struct {
	Index           int     `json:"index"`
	BasePTS         int64   `json:"base_pts"`
	DurationTicks   int64   `json:"duration_ticks"`
	Bytes           int     `json:"bytes"`
	TargetBPS       int64   `json:"target_bps"`
	ActualBPS       int64   `json:"actual_bps"`
	DeviationPct    float64 `json:"deviation_pct"`
	WithinTolerance bool    `json:"within_tolerance"`
}

// Report contains the acceptance metrics printed by the CLI.
type Report struct {
	TargetBPS       int64           `json:"target_bps"`
	ActualBPS       int64           `json:"actual_bps"`
	DeviationPct    float64         `json:"deviation_pct"`
	AudioLUFS       float64         `json:"audio_lufs"`
	TruePeakDBFS    float64         `json:"true_peak_dbfs"`
	ManifestSkewUS  int64           `json:"manifest_skew_us"`
	FrameCount      int             `json:"frame_count"`
	SHA256          string          `json:"sha256"`
	Segments        []SegmentReport `json:"segments"`
	ProcessedOffset int64           `json:"processed_offset"`
	QueueHighWater  int             `json:"queue_high_water_bytes"`
}

// Bitrate returns bits/sec for a byte count and 90 kHz duration.
func Bitrate(byteCount int64, durationTicks int64) int64 {
	if durationTicks <= 0 {
		return 0
	}
	return byteCount * 8 * media.ClockRate / durationTicks
}

// DeviationPercent returns signed percentage deviation from target.
func DeviationPercent(target, actual int64) float64 {
	if target == 0 {
		return 0
	}
	return (float64(actual-target) / float64(target)) * 100
}

// CheckSegment compares actual fMP4 bytes against the declared target.
func CheckSegment(index int, targetBPS int64, seg mux.SegmentInfo) SegmentReport {
	actual := Bitrate(int64(seg.Bytes), seg.DurationTicks)
	dev := DeviationPercent(targetBPS, actual)
	return SegmentReport{
		Index: index, BasePTS: seg.BasePTS, DurationTicks: seg.DurationTicks,
		Bytes: seg.Bytes, TargetBPS: targetBPS, ActualBPS: actual,
		DeviationPct:    dev,
		WithinTolerance: absFloat(dev) <= BitrateTolerancePercent,
	}
}

// CheckSegments checks every keyframe segment and returns a typed failure if
// any segment exceeds tolerance. It never claims video was re-encoded.
func CheckSegments(targetBPS int64, segments []mux.SegmentInfo) ([]SegmentReport, error) {
	out := make([]SegmentReport, 0, len(segments))
	for i, seg := range segments {
		r := CheckSegment(i, targetBPS, seg)
		out = append(out, r)
		if !r.WithinTolerance {
			return out, media.E(media.StageMetrics, media.KindValidation, seg.BasePTS,
				fmt.Errorf("segment %d bitrate deviation %.2f%% exceeds %.1f%%",
					i, r.DeviationPct, BitrateTolerancePercent))
		}
	}
	return out, nil
}

// Overall computes container bitrate and deviation.
func Overall(targetBPS int64, totalBytes int64, durationTicks int64) (actual int64, deviation float64) {
	actual = Bitrate(totalBytes, durationTicks)
	return actual, DeviationPercent(targetBPS, actual)
}

// Stats tracks frame/byte/queue counters.
type Stats struct {
	FrameCount      int
	KeyframeCount   int
	InputBytes      int64
	OutputBytes     int64
	ProcessedOffset int64
	QueueHighWater  int
}

// ObserveQueue records queue byte high-water mark.
func (s *Stats) ObserveQueue(byteLen int) {
	if byteLen > s.QueueHighWater {
		s.QueueHighWater = byteLen
	}
}

func absFloat(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
