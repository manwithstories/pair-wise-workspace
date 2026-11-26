// Package pipeline schedules the restoration stages and aggregates their
// results into one report. The data flow is: WAV parse -> block check and
// parity recovery -> timeline/graph -> frame-boundary cut -> silence
// detection -> fingerprint dedupe -> quantified summary. All stages share
// the model rational time base and the same structured error log.
package pipeline

import (
	"time"

	"audiorestore/internal/fingerprint"
	"audiorestore/internal/model"
	"audiorestore/internal/report"
	"audiorestore/internal/segment"
	"audiorestore/internal/silence"
	"audiorestore/internal/timeline"
	"audiorestore/internal/wav"
)

// Carrier is one in-process input: the rendered WAV bytes plus the
// timeline placement and optional parity group size (0 = no parity).
type Carrier struct {
	WAV          []byte
	ID           string
	StartFrame   int64
	DependsOn    []string
	DataPerGroup int
}

// Config controls the analysis stages.
type Config struct {
	WindowFrames  int64
	KeepPartial   bool
	SilenceThresh int
	SilenceMinRun int64
}

// TrackResult holds one carrier's stage outputs.
type TrackResult struct {
	Track      *timeline.Track
	Blocks     []wav.FrameBlock
	Segments   []segment.Segment
	Silence    []silence.Range
	Recovered  []int
	Unrecov    []int
	BlockTimes []time.Duration
	StageTimes []time.Duration
}

// Run is the complete pipeline over carriers and video anchors.
type Run struct {
	carriers []Carrier
	anchors  []timeline.Anchor
	refRate  model.SampleRate
	cfg      Config
	log      *model.ErrorLog
	table    *fingerprint.Table
}

// New constructs a pipeline run.
func New(carriers []Carrier, anchors []timeline.Anchor, refRate model.SampleRate, cfg Config) *Run {
	if cfg.WindowFrames <= 0 {
		cfg.WindowFrames = 128
	}
	return &Run{
		carriers: carriers,
		anchors:  anchors,
		refRate:  refRate,
		cfg:      cfg,
		log:      model.NewErrorLog(),
		table:    fingerprint.NewTable(),
	}
}

// Execute runs every stage in dependency order and builds the summary.
func (r *Run) Execute() (*report.Summary, error) {
	tracks, results, err := r.processCarriers()
	if err != nil {
		return nil, err
	}
	resByID := make(map[string]*TrackResult, len(results))
	for _, res := range results {
		resByID[res.Track.ID] = res
	}
	if r.noCompleteFrames(results) {
		return nil, model.TrackedError{Kind: model.ErrNoFrames,
			Msg: "no complete sample frames recovered from any carrier"}
	}
	g, err := timeline.NewGraph(tracks, r.anchors, r.refRate)
	if err != nil {
		return nil, err
	}
	timeline.CheckLayout(g, r.log)
	timeline.CheckColor(g, r.log)
	if err := r.analyze(g, resByID); err != nil {
		return nil, err
	}
	return r.summarize(g, results), nil
}

// Log exposes the structured error log (degradation evidence).
func (r *Run) Log() *model.ErrorLog { return r.log }

func (r *Run) noCompleteFrames(results []*TrackResult) bool {
	for _, res := range results {
		for i := range res.Blocks {
			b := &res.Blocks[i]
			if !b.Parity && !b.Missing && !b.Corrupt && !b.Truncated {
				return false
			}
		}
	}
	return true
}
