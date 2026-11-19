package recover

import (
	"archivearchivist/internal/model"
	"archivearchivist/internal/mp4"
)

// findRanges is completed with keyframe bracketing in the next change.
func findRanges(trackID string, sc *mp4.Scan, cfg Config) []Range {
	return nil
}

// recoverClip is completed with keyframe snapping in the next change.
func recoverClip(c model.Clip, in Input, cfg Config) []Boundary {
	return []Boundary{
		{Clip: c.ID, Track: c.Track, Edge: "in", Intended: c.Start, Keyframe: -1, ErrorUS: -1, Status: "blocked"},
		{Clip: c.ID, Track: c.Track, Edge: "out", Intended: c.Start.Add(c.Duration), Keyframe: -1, ErrorUS: -1, Status: "blocked"},
	}
}

// recoverTransition is completed with crossfade comparison in the next change.
func recoverTransition(x model.Transition, bs []Boundary) TransitionResult {
	return TransitionResult{ID: x.ID, From: x.From, To: x.To, Status: "blocked"}
}
