package transport

import "math"

// BandwidthSegment is one independently timed media segment.
type BandwidthSegment struct {
	ID         int
	DurationNS int64
	Bytes      int64
}

// BandwidthReport compares requested and measured payload throughput.
type BandwidthReport struct {
	TargetKBPS   int                `json:"target_kbps"`
	TargetBPS    float64            `json:"target_bps"`
	ActualBPS    float64            `json:"actual_bps"`
	ActualKBPS   float64            `json:"actual_kbps"`
	DeviationPct float64            `json:"deviation_pct"`
	Segments     []BandwidthSegment `json:"segments"`
	SegmentKBPS  []float64          `json:"segment_kbps"`
	TotalBytes   int64              `json:"total_bytes"`
	DurationNS   int64              `json:"duration_ns"`
}

// MeasureBandwidth computes the aggregate and per-segment bitrate trajectory.
func MeasureBandwidth(targetKBPS int, segments []BandwidthSegment) BandwidthReport {
	r := BandwidthReport{TargetKBPS: targetKBPS, Segments: append([]BandwidthSegment(nil), segments...)}
	r.TargetBPS = float64(targetKBPS) * 1000
	for _, s := range segments {
		r.TotalBytes += s.Bytes
		r.DurationNS += s.DurationNS
		if s.DurationNS > 0 {
			r.SegmentKBPS = append(r.SegmentKBPS, float64(s.Bytes)*8.0*1e9/float64(s.DurationNS)/1000.0)
		} else {
			r.SegmentKBPS = append(r.SegmentKBPS, 0)
		}
	}
	if r.DurationNS > 0 {
		r.ActualBPS = float64(r.TotalBytes) * 8.0 * 1e9 / float64(r.DurationNS)
		r.ActualKBPS = r.ActualBPS / 1000.0
		if r.TargetBPS != 0 {
			r.DeviationPct = math.Abs(r.ActualBPS-r.TargetBPS) / r.TargetBPS * 100.0
		}
	}
	return r
}

// WithinTolerance reports whether the aggregate rate is at most pct different.
func (r BandwidthReport) WithinTolerance(pct float64) bool {
	return r.DeviationPct <= pct
}
