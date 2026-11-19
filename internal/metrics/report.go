// Package metrics turns a pipeline Result into a quantified, reproducible
// terminal summary and evaluates the numeric assertions that decide the
// process exit code. Thresholds are explicit so a run fails on a measured
// ratio rather than on a log message.
package metrics

import (
	"fmt"
	"io"
	"strings"

	"mediapipeline/internal/pipeline"
)

// Thresholds collects the quantitative pass conditions for a run.
type Thresholds struct {
	// MinRoundTrip is the minimum end-to-end delivery/consistency ratio.
	MinRoundTrip float64
	// MinKeyframeHit is the minimum keyframe index hit rate.
	MinKeyframeHit float64
	// MaxBufferBytes bounds the observed high-water mark (already inclusive
	// of the 20% fixed-overhead allowance when supplied by the caller).
	MaxBufferBytes int64
}

// DefaultThresholds matches the acceptance criteria: strong round-trip and
// keyframe guarantees while still allowing the configured weak-network loss.
func DefaultThresholds() Thresholds {
	return Thresholds{MinRoundTrip: 0.90, MinKeyframeHit: 1.0, MaxBufferBytes: 0}
}

// Summary is the evaluated report.
type Summary struct {
	RoundTripConsistency float64
	KeyframeHitRate      float64
	SegmentCount         int
	SegmentHashes        []string
	PeakBufferBytes      int64
	DroppedPackets       int64
	LatePackets          int64
	PartialFailures      int64
	Passed               bool
	Failures             []string
}

// Evaluate computes ratios from the result and checks every threshold.
func Evaluate(res *pipeline.Result, expectedMediaPackets int, th Thresholds) Summary {
	s := Summary{
		SegmentCount:    len(res.Segments),
		PeakBufferBytes: res.PeakBufferBytes,
		DroppedPackets:  res.Counters.Dropped,
		LatePackets:     res.Counters.Late,
		PartialFailures: res.Counters.PartialFailures,
	}
	if expectedMediaPackets > 0 {
		s.RoundTripConsistency = float64(res.Counters.Parsed) / float64(expectedMediaPackets)
	}
	var hitSum, hitSegs float64
	for _, seg := range res.Segments {
		s.SegmentHashes = append(s.SegmentHashes, seg.SHA256)
		if seg.VideoSamples > 0 {
			hitSum += seg.KeyframeHitRate
			hitSegs++
		}
	}
	if hitSegs > 0 {
		s.KeyframeHitRate = hitSum / hitSegs
	} else {
		s.KeyframeHitRate = 1.0 // no video index to violate: vacuously satisfied
	}
	if s.RoundTripConsistency < th.MinRoundTrip {
		s.Failures = append(s.Failures, fmt.Sprintf(
			"roundtrip_consistency=%.4f below threshold %.2f",
			s.RoundTripConsistency, th.MinRoundTrip))
	}
	if s.KeyframeHitRate < th.MinKeyframeHit {
		s.Failures = append(s.Failures, fmt.Sprintf(
			"keyframe_hit_rate=%.3f below threshold %.3f",
			s.KeyframeHitRate, th.MinKeyframeHit))
	}
	if th.MaxBufferBytes > 0 && s.PeakBufferBytes > th.MaxBufferBytes {
		s.Failures = append(s.Failures, fmt.Sprintf(
			"peak_buffer_bytes=%d exceeds limit %d",
			s.PeakBufferBytes, th.MaxBufferBytes))
	}
	s.Passed = len(s.Failures) == 0
	return s
}

// WriteText emits the stable key=value summary consumed by the CLI and tests.
func WriteText(w io.Writer, s Summary) {
	fmt.Fprintf(w, "roundtrip_consistency=%.4f\n", s.RoundTripConsistency)
	fmt.Fprintf(w, "keyframe_hit_rate=%.3f\n", s.KeyframeHitRate)
	fmt.Fprintf(w, "segments=%d\n", s.SegmentCount)
	for i, h := range s.SegmentHashes {
		fmt.Fprintf(w, "segment_sha256[%d]=%s\n", i, h)
	}
	// The acceptance field name segment_sha256=<64 hex> is also emitted in
	// its singular form for the first segment, matching the documented key.
	if len(s.SegmentHashes) > 0 {
		fmt.Fprintf(w, "segment_sha256=%s\n", s.SegmentHashes[0])
	}
	fmt.Fprintf(w, "peak_buffer_bytes=%d\n", s.PeakBufferBytes)
	fmt.Fprintf(w, "dropped_packets=%d\n", s.DroppedPackets)
	fmt.Fprintf(w, "late_packets=%d\n", s.LatePackets)
	fmt.Fprintf(w, "partial_failures=%d\n", s.PartialFailures)
	if len(s.Failures) > 0 {
		fmt.Fprintf(w, "assertion_failures=%s\n", strings.Join(s.Failures, "; "))
	}
	fmt.Fprintf(w, "passed=%t\n", s.Passed)
}
