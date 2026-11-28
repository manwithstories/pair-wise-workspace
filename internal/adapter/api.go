// Package adapter defines the versioned media-probing interface. Adapters
// emit media samples carrying their source byte offset so the recovery
// layer can reason about damaged regions without re-reading the file.
package adapter

import (
	"context"

	"presspack/internal/clock"
	"presspack/internal/core"
)

// APIVersion identifies the interface revision implemented here.
const APIVersion = "v1"

// MediaSample is one access unit with its position in the source file.
type MediaSample struct {
	PTS      core.Time // presentation time on the unified time base
	Duration core.Time // sample duration (0 when unknown)
	Keyframe bool
	Offset   int64 // byte offset in the source (-1 when unknown)
}

// ProbeResult is the adapter output for one input file.
type ProbeResult struct {
	Path      string
	Format    string // container family: "mp4", "mpegts", ...
	Duration  core.Time
	FPS       clock.FPS
	Width     int
	Height    int
	BitRate   int64 // container average bit rate, bits/s
	Samples   []MediaSample
	Faults    core.Faults
	Truncated bool // stream ends before its declared duration
}

// Keyframes returns the presentation times of all keyframe samples.
func (p *ProbeResult) Keyframes() []core.Time {
	var out []core.Time
	for _, s := range p.Samples {
		if s.Keyframe {
			out = append(out, s.PTS)
		}
	}
	return out
}

// Adapter probes one local media file. Implementations are versioned
// (ffprobe_v1 etc.) and must be safe for concurrent use.
type Adapter interface {
	Version() string
	Probe(ctx context.Context, path string) (*ProbeResult, error)
}
