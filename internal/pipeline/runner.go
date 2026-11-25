package pipeline

import (
	"mediad/internal/checkpoint"
	"mediad/internal/codec"
	"mediad/internal/cut"
	"mediad/internal/dedup"
	"mediad/internal/sample"
	"mediad/internal/timeline"
)

type runner struct {
	opts             Options
	stream           *sample.Stream
	spans            []timeline.Span
	reports          []codec.Report
	journal          *checkpoint.Journal
	journalPath      string
	manifestPath     string
	initName         string
	manifestName     string
	cutter           *cut.Cutter
	store            *dedup.Store
	contents         map[string]dedup.Content
	flow             *rtpFlow
	mem              *byteBudget
	manifestSegments []ManifestSegment
	alignMax         int64
	actualBytes      int64
	firstErrorByte   int64
	pausedReads      int64
	crossNodeChecked bool
}
