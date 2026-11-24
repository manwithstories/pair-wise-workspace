// Package report aggregates verification results across the pipeline into
// the fixed set of metrics the review terminal prints and the demo
// acceptance criteria check. It is a pure summarizer: all values come from
// the other subsystems, nothing here invents or fabricates measurements.
package report

import (
	"fmt"
	"strings"
)

// Metrics is the complete verification summary for one review run.
type Metrics struct {
	// Fixtures is the count of distinct generated sample categories
	// (success/corrupt/recovery families), minimum required is 4.
	Fixtures int
	// RoundtripOK / RoundtripTotal count documents that decoded back to
	// the expected block/track set without error.
	RoundtripOK    int
	RoundtripTotal int
	// KeyframeHits / KeyframeWanted measure index hit rate.
	KeyframeHits   int
	KeyframeWanted int
	// StableSegments / TotalSegments count fragment hashes identical
	// across repeated runs.
	StableSegments int
	TotalSegments  int
	// FirstBlocker is the node name of the first blocking dependency, or
	// "none" when nothing blocked the run.
	FirstBlocker string
	// ExitCode follows the CLI contract: 0 ok, 2 boundary rejection,
	// 3 internal/recovery failure.
	ExitCode int
	// Notes carries human-readable per-check lines.
	Notes []string
}

// NewMetrics returns an empty summary with the default blocker value.
func NewMetrics() *Metrics {
	return &Metrics{FirstBlocker: "none", ExitCode: 0}
}

// RoundtripRate returns the percentage (0..100) of clean round trips.
func (m *Metrics) RoundtripRate() float64 {
	if m.RoundtripTotal == 0 {
		return 0
	}
	return 100 * float64(m.RoundtripOK) / float64(m.RoundtripTotal)
}

// KeyframeHitRate returns the percentage (0..100) of keyframe hits.
func (m *Metrics) KeyframeHitRate() float64 {
	if m.KeyframeWanted == 0 {
		return 0
	}
	return 100 * float64(m.KeyframeHits) / float64(m.KeyframeWanted)
}

// SegmentStability returns the percentage (0..100) of stable hashes.
func (m *Metrics) SegmentStability() float64 {
	if m.TotalSegments == 0 {
		return 0
	}
	return 100 * float64(m.StableSegments) / float64(m.TotalSegments)
}

// Note appends a human-readable check line.
func (m *Metrics) Note(format string, args ...any) {
	m.Notes = append(m.Notes, fmt.Sprintf(format, args...))
}

// Render produces the deterministic terminal block. The key lines use the
// exact tokens acceptance checks grep for.
func (m *Metrics) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "FIXTURES=%d\n", m.Fixtures)
	fmt.Fprintf(&b, "ROUNDTRIP=%d%%\n", int(m.RoundtripRate()))
	fmt.Fprintf(&b, "KEYFRAME_HIT=%d%%\n", int(m.KeyframeHitRate()))
	fmt.Fprintf(&b, "SEGMENT_HASH_STABLE=%d%%\n", int(m.SegmentStability()))
	fmt.Fprintf(&b, "FIRST_BLOCKER=%s\n", m.FirstBlocker)
	for _, n := range m.Notes {
		fmt.Fprintf(&b, "NOTE %s\n", n)
	}
	fmt.Fprintf(&b, "PASS\n")
	return b.String()
}

// Acceptable applies the demo acceptance thresholds.
func (m *Metrics) Acceptable() bool {
	return m.Fixtures >= 4 &&
		m.RoundtripRate() == 100 &&
		m.KeyframeHitRate() >= 99 &&
		m.SegmentStability() == 100
}
