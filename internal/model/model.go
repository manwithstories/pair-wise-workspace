// Package model defines the shared event and error model plus the
// resource limits enforced across all modules.
package model

import (
	"fmt"

	"cueforge/internal/clock"
)

// Resource limits. Inputs exceeding them are rejected.
const (
	// MaxRecordBytes caps a single parsed record (cue, box, NAL, line).
	MaxRecordBytes = 64 << 10
	// MaxTracks caps the number of input tracks.
	MaxTracks = 64
	// MaxEvents caps the total number of timeline events.
	MaxEvents = 1 << 20
	// MaxBufferedBytes caps total bytes buffered in pipeline queues.
	MaxBufferedBytes = 2 << 20
	// CommitBatch is the maximum number of events between checkpoints.
	CommitBatch = 64
)

// Stage identifies the pipeline stage reporting an error.
type Stage string

const (
	StageParse      Stage = "parse"
	StageMerge      Stage = "merge"
	StageValidate   Stage = "validate"
	StageRender     Stage = "render"
	StageCheckpoint Stage = "checkpoint"
)

// TrackError preserves the original failure location: track, stage,
// source byte offset and media time.
type TrackError struct {
	Track  int
	Stage  Stage
	Offset int64
	Time   clock.Time
	Msg    string
}

func (e *TrackError) Error() string {
	return fmt.Sprintf("track=%d stage=%s offset=%d time=%s: %s",
		e.Track, e.Stage, e.Offset, e.Time, e.Msg)
}

// Event is one timeline record parsed from a track.
type Event struct {
	Track   int
	Seq     int64
	Start   clock.Time
	Dur     clock.Time
	Kind    string // caption, chapter, id3, emsg, sei
	Payload string
	Offset  int64 // source byte offset of the record
}

// Size approximates the in-memory footprint for buffer budgeting.
func (e Event) Size() int64 { return int64(len(e.Payload)) + 96 }
