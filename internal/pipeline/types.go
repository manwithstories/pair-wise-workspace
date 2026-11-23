package pipeline

import (
	"fmt"

	"mediasync/internal/manifest"
)

// Options configure a run.
type Options struct {
	// InputDir holds the generated fixtures.
	InputDir string
	// PackageDir receives the media package and manifests.
	PackageDir string
	// TargetKbps is the declared budget; the measured rate is reported apart.
	TargetKbps int
	// BandwidthKbps seeds the estimator before the first block.
	BandwidthKbps int
	// CheckpointSamples is the fixed block size in samples.
	CheckpointSamples int
	// Resume reuses the existing checkpoint directory.
	Resume bool
	// InterruptAfter stops after this many blocks and returns Interrupted.
	InterruptAfter int
	// Rotation is an explicit rotation override in degrees.
	Rotation *int
	// Seed reproduces the generated inputs.
	Seed int64
	// LossBurst, Duplicate and Reorder describe the RTP stream impairments.
	LossBurst, Duplicate, Reorder int
	// Corrupt selects a damaged input fixture by name.
	Corrupt string
}

// Report is the run summary the CLI prints.
type Report struct {
	TrackCount          int
	SampleCount         int
	TargetKbps          int
	ActualKbps          float64
	BitrateErrPct       float64
	SampleSHAMatch      bool
	QualityMean         float64
	QualityP95          float64
	ManifestAlignMs     float64
	CheckpointBlocks    int
	RecoveredBlocks     int
	CheckpointRecovered bool
	FinalHash           string
	Incomplete          []manifest.Interval
	Warnings            []string
	// RTP tuning statistics, reported for the transfer operator.
	RTPReceived, RTPDuplicates, RTPOutOfOrder int
}

// InterruptError signals a deliberate stop after InterruptAfter blocks.
type InterruptError struct{ After int }

func (e *InterruptError) Error() string {
	return fmt.Sprintf("interrupted after %d checkpoint blocks", e.After)
}
