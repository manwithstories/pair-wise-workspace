// Package worker runs the rewrap pipeline with bounded concurrency and
// bounded memory. Each segment is probed, indexed, normalised and hashed
// independently; results are merged in segment order and checkpointed per
// fixed-size chunk so an interrupted run resumes deterministically.
package worker

import (
	"errors"
	"fmt"

	"mediad/internal/audio"
	"mediad/internal/checkpoint"
	"mediad/internal/fault"
	segmenthash "mediad/internal/hash"
	"mediad/internal/index"
	"mediad/internal/manifest"
	"mediad/internal/probe"
)

// SegmentInput is everything the pipeline needs for one segment.
type SegmentInput struct {
	Segment manifest.Segment
	// Media is the probed structure, or nil when the segment is unreadable.
	Media *probe.MediaInfo
	// Timescales maps track IDs to the init segment timescales.
	Timescales map[uint32]uint32
	// Audio is the decoded source, if this representation carries audio.
	Audio *audio.Source
	// InputHash is the SHA-256 of the raw segment bytes.
	InputHash segmenthash.Hex
	// InputSize is the raw byte size.
	InputSize int64
	// ProbeFaults are structural faults observed before processing.
	ProbeFaults []fault.Fault
}

// Output is the per-segment result.
type Output struct {
	URI         string
	Sequence    uint64
	StartTicks  int64
	EndTicks    int64
	Determined  bool
	InputHash   segmenthash.Hex
	OutputHash  segmenthash.Hex
	OutputSize  int64
	Keyframes   []index.Entry
	Gap         bool
	Faults      []fault.Fault
	AudioResult *audio.Result
	Chunk       int
	InputKnown  bool
	OutputKnown bool
}

// Options configures the pipeline.
type Options struct {
	ChunkSize        int
	Workers          int
	MemoryLimitBytes int64
	Target           audio.Target
}

// DefaultOptions returns bounded defaults.
func DefaultOptions() Options {
	return Options{ChunkSize: 8, Workers: 4, Target: audio.DefaultTarget(), MemoryLimitBytes: 64 << 20}
}

// Validate rejects unbounded or nonsensical settings.
func (o Options) Validate() error {
	if err := checkpoint.Validate(o.ChunkSize); err != nil {
		return err
	}
	if o.Workers <= 0 || o.Workers > 256 {
		return fmt.Errorf("worker: workers %d out of range", o.Workers)
	}
	if o.MemoryLimitBytes < 0 {
		return fmt.Errorf("worker: negative memory limit")
	}
	return o.Target.Validate()
}

// Result is the merged pipeline result.
type Result struct {
	Outputs     []Output
	Faults      []fault.Fault
	KeyIndex    *index.Index
	Chunks      int
	Recovered   int
	ManifestSHA string
}

// SegmentsOK counts determined outputs without a gap.
func (r *Result) SegmentsOK() int {
	n := 0
	for _, o := range r.Outputs {
		if o.Determined && !o.Gap {
			n++
		}
	}
	return n
}

// SegmentsBad counts outputs that are gapped or undetermined.
func (r *Result) SegmentsBad() int { return len(r.Outputs) - r.SegmentsOK() }

// TimelineGaps counts outputs that preserved a hole in the timeline.
func (r *Result) TimelineGaps() int {
	n := 0
	for _, o := range r.Outputs {
		if o.Gap {
			n++
		}
	}
	return n
}

// ErrNoInputs is returned when the pipeline is invoked with nothing to do.
var ErrNoInputs = errors.New("worker: no inputs")

// EnsureInputs validates the caller passes a non-empty workload.
func EnsureInputs(inputs []SegmentInput) error {
	if len(inputs) == 0 {
		return ErrNoInputs
	}
	return nil
}
