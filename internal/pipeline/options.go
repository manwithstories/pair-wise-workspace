package pipeline

import (
	"mediad/internal/checkpoint"
	"mediad/internal/codec"
	"mediad/internal/sample"
	"mediad/internal/silence"
)

// Options configures one codec pipeline run.
type Options struct {
	OutDir          string
	Codec           codec.Codec
	Media           *sample.Media
	Resume          bool
	InjectDuplicate bool
	InterruptAfter  int
	QueueCapacity   int
}

// Result contains deterministic output locations and quality statistics.
type Result struct {
	Manifest       *Manifest
	ManifestPath   string
	ManifestSHA256 string
	JournalPath    string
	Journal        *checkpoint.Journal
	Reports        []codec.Report
	Silence        silence.Result
	ActualBPS      int64
	DeltaPct       float64
	RTPDuplicates  int64
	PausedReads    int64
	FirstErrorByte int64
	Interrupted    bool
}
