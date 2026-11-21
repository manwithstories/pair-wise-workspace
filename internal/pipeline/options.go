package pipeline

import (
	"mediad/internal/fixture"
	"mediad/internal/manifest"
	"mediad/internal/report"
	"mediad/internal/txn"
)

// DemoOptions configures the demo run.
type DemoOptions struct {
	Seed           int64
	Faults         []fixture.FaultKind
	BandwidthMbps  []float64
	InterruptAfter txn.Stage
	Resume         bool
	Dir            string
	SeeksPerTrack  int
	MaxBurst       int
}

// Result is everything the CLI prints and gates on.
type Result struct {
	Summary        report.Summary
	Manifest       manifest.Manifest
	RollbackStage  string
	ResumeStage    string
	InjectedFaults int
	Notes          []string
	ReusedStages   []string
}
