// Package pipeline orchestrates the processing stages: manifest entries
// flow through the bounded reader, hash verifier and bitstream scanner;
// valid data flows through video/audio/color processors over
// fixed-capacity channels that apply backpressure upstream.
package pipeline

import (
	"context"

	"mediad/internal/color"
	"mediad/internal/fault"
	"mediad/internal/manifest"
	"mediad/internal/timeline"
)

// Options configures one run.
type Options struct {
	Dir          string // base directory for track files
	TargetBPS    int64
	QueueCap     int // channel capacity; <=0 means 1
	Workers      int // proxy stage workers; bounded to 1 (ordered feedback)
	MaxKeyGap    int // maximum keyframe interval in frames
	MaxFileBytes int64
}

func (o *Options) defaults() {
	if o.QueueCap <= 0 {
		o.QueueCap = 1
	}
	// The proxy stage runs a single ordered encoder so that bitrate
	// feedback is deterministic; Workers is bounded to that limit.
	o.Workers = 1
	if o.MaxKeyGap <= 0 {
		o.MaxKeyGap = 48
	}
	if o.MaxFileBytes <= 0 {
		o.MaxFileBytes = 64 << 20
	}
}

// FrameStat holds per-frame measurements of one processed frame.
type FrameStat struct {
	Track string
	Seq   int64
	PTS   int64
	Bytes int64
	MSE   float64
	PSNR  float64
}

// Result aggregates one manifest run.
type Result struct {
	Frames          int
	FrameStats      []FrameStat // sorted by sequence number
	ProxyBytes      int64
	DurationSec     float64
	AlignMaxMs      float64
	CorruptRanges   []timeline.HalfOpen
	RecoveredFrames int
	AudioSamples    map[string]int64
	ColorActions    map[string]color.Action
	Faults          []fault.Fault
}

// Run processes every track of m. Local I/O failures are returned as
// errors; media defects are recorded as faults and isolated.
func Run(ctx context.Context, m *manifest.Manifest, opts Options) (*Result, error) {
	opts.defaults()
	clock, err := timeline.New(m.Timebase)
	if err != nil {
		return nil, err
	}
	res := &Result{
		AudioSamples: map[string]int64{},
		ColorActions: map[string]color.Action{},
	}
	var faults fault.List
	for i := range m.Tracks {
		t := &m.Tracks[i]
		var err error
		switch t.Kind {
		case "video":
			err = runVideo(ctx, clock, t, opts, res, &faults)
		case "audio":
			err = runAudio(t, opts, res, &faults)
		}
		if err != nil {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	res.Faults = faults.Items()
	res.CorruptRanges = buildRanges(clock, m, res.Faults)
	res.RecoveredFrames = countRecovered(res)
	return res, nil
}
