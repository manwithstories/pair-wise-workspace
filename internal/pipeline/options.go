// Package pipeline wires the three subsystems together: transport (packets
// to the unified clock), parse/index (media to keyframe index and timed
// events) and composition (merged timeline to committed segments and review
// frames).
package pipeline

import (
	"mediarun/internal/testkit"
)

// Options are the demo inputs.
type Options struct {
	Seed      int64
	Scenario  testkit.Scenario
	Bandwidth []int
	Out       string
	Window    int
}

// Result is the acceptance summary printed by the CLI.
type Result struct {
	RoundTripRate  float64
	KeyframeRate   float64
	Segments       int
	SegmentHashes  []string
	DetectedGaps   int
	RollbackCount  int
	Recovered      int
	CommittedMatch int
	BandwidthSteps []int
	Frames         int
	Events         int
	CorruptCases   int
	CorruptCaught  int
	GapSummary     string
	Scenario       string
}
