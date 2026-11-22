// Package model holds the unified, format-independent description of the
// material medialine processes: tracks of timed samples, edit points, and the
// transition regions that link two source tracks.
package model

import (
	"fmt"
	"sort"

	"medialine/internal/fault"
)

// TrackKind distinguishes video from audio material.
type TrackKind string

const (
	KindVideo TrackKind = "video"
	KindAudio TrackKind = "audio"
)

// Sample is one timed access unit on a track. Times are on the unified
// microsecond timeline. Offset is the byte offset of the sample payload inside
// the source file identified by Track.Source.
type Sample struct {
	Number     int
	DTSUS      int64
	PTSUS      int64
	DurationUS int64
	Size       int
	Offset     int64
	Keyframe   bool
}

// Track groups samples that share an ID and clock origin.
type Track struct {
	ID        int
	Kind      TrackKind
	Timescale uint32
	Source    string
	Codec     string
	Samples   []Sample
}

// DurationUS returns the presentation duration covered by the track.
func (t *Track) DurationUS() int64 {
	if len(t.Samples) == 0 {
		return 0
	}
	last := t.Samples[len(t.Samples)-1]
	return last.PTSUS + last.DurationUS
}

// Validate checks ordering invariants that later stages rely on. The first
// offending sample is reported with the track ID and its sample offset.
func (t *Track) Validate() error {
	for i := range t.Samples {
		s := t.Samples[i]
		if s.DurationUS < 0 {
			return fault.New(fault.StageParse, fault.KindZeroDuration,
				"negative duration %dus", s.DurationUS).At(t.ID, int64(i))
		}
		if i > 0 {
			prev := t.Samples[i-1]
			if s.DTSUS < prev.DTSUS {
				return fault.New(fault.StageParse, fault.KindBackwardsTime,
					"sample dts %dus precedes %dus", s.DTSUS, prev.DTSUS).At(t.ID, int64(i))
			}
			if s.DTSUS == prev.DTSUS && s.PTSUS == prev.PTSUS {
				return fault.New(fault.StageParse, fault.KindDuplicateTime,
					"duplicate sample timestamp %dus", s.PTSUS).At(t.ID, int64(i))
			}
		}
	}
	return nil
}

// KeyframeIndex returns sample numbers (1-based) that are sync samples.
func (t *Track) KeyframeIndex() []int {
	var out []int
	for _, s := range t.Samples {
		if s.Keyframe {
			out = append(out, s.Number)
		}
	}
	return out
}

// TransitionKind describes how a transition region should be assembled.
type TransitionKind string

const (
	TransitionNone  TransitionKind = ""
	TransitionCross TransitionKind = "crossfade"
	TransitionWipe  TransitionKind = "wipe"
)

// Transition is an interval [StartUS, EndUS) during which the output draws on
// more than one source track. The primary track is the one the segment is
// reported against; Sources lists every track involved, ordered.
type Transition struct {
	StartUS int64
	EndUS   int64
	Kind    TransitionKind
	Sources []int
}

// Validate checks the region is non-empty and references known tracks.
func (tr *Transition) Validate(known map[int]bool) error {
	if tr.EndUS <= tr.StartUS {
		return fault.New(fault.StageAlign, fault.KindZeroDuration,
			"transition end %dus is not after start %dus", tr.EndUS, tr.StartUS)
	}
	if len(tr.Sources) == 0 {
		return fault.New(fault.StageAlign, fault.KindBadConfig, "transition has no source tracks")
	}
	for _, id := range tr.Sources {
		if !known[id] {
			return fault.New(fault.StageAlign, fault.KindBadConfig, "transition references unknown track %d", id)
		}
	}
	return nil
}

// Cut is an operator-specified edit point on the unified timeline.
type Cut struct {
	AtUS       int64
	Name       string
	Transition *Transition
}

// EditPoint is a cut resolved against material, with the effective boundary
// after keyframe snapping. SnapUS is the signed adjustment from the requested
// cut: negative when the boundary was pulled earlier.
type EditPoint struct {
	RequestedUS int64
	EffectiveUS int64
	SnapUS      int64
	Snapped     bool
	SegmentID   string
}

// SegmentKind classifies a manifest entry.
type SegmentKind string

const (
	SegmentSimple    SegmentKind = "simple"
	SegmentComposite SegmentKind = "composite"
	SegmentGap       SegmentKind = "gap"
)

// Segment is one slice of the timeline for one representation. It is the unit
// that the manifest, writer and hash verifier all agree on.
type Segment struct {
	ID               string
	RepresentationID string
	TrackID          int
	Kind             SegmentKind
	StartUS          int64
	EndUS            int64
	TransitionKind   TransitionKind
	SourceTrackIDs   []int
	// SampleNumbers are 1-based sample numbers of TrackID included in order.
	SampleNumbers []int
	// SnapOffsetUS records the keyframe snap applied when the requested
	// boundary did not land on a sample start.
	SnapOffsetUS int64
	// File is the relative output path of the segment payload.
	File string
	// ContentHash is filled after the segment has been written and verified.
	ContentHash string
}

// DurationUS returns the segment span.
func (s Segment) DurationUS() int64 { return s.EndUS - s.StartUS }

// Timeline is the complete model passed between pipeline stages.
type Timeline struct {
	Tracks []*Track
	Cuts   []Cut
	// DurationUS is the overall presentation span; zero means derive from the
	// longest track.
	DurationUS int64
}

// Track looks up a track by ID.
func (tl *Timeline) Track(id int) (*Track, bool) {
	for _, t := range tl.Tracks {
		if t.ID == id {
			return t, true
		}
	}
	return nil, false
}

// KnownTracks returns the set of track IDs.
func (tl *Timeline) KnownTracks() map[int]bool {
	m := make(map[int]bool, len(tl.Tracks))
	for _, t := range tl.Tracks {
		m[t.ID] = true
	}
	return m
}

// Span returns the timeline duration, deriving it from tracks when unset.
func (tl *Timeline) Span() int64 {
	if tl.DurationUS > 0 {
		return tl.DurationUS
	}
	var max int64
	for _, t := range tl.Tracks {
		if d := t.DurationUS(); d > max {
			max = d
		}
	}
	return max
}

// Validate checks tracks, cuts and transitions together, returning the first
// fault found.
func (tl *Timeline) Validate() error {
	known := tl.KnownTracks()
	ids := make(map[int]bool, len(tl.Tracks))
	for _, t := range tl.Tracks {
		if t.ID <= 0 {
			return fault.New(fault.StageParse, fault.KindBadConfig, "track id must be positive, got %d", t.ID)
		}
		if ids[t.ID] {
			return fault.New(fault.StageParse, fault.KindBadConfig, "duplicate track id %d", t.ID)
		}
		ids[t.ID] = true
		if err := t.Validate(); err != nil {
			return err
		}
	}
	cuts := append([]Cut(nil), tl.Cuts...)
	sort.SliceStable(cuts, func(i, j int) bool { return cuts[i].AtUS < cuts[j].AtUS })
	for i := range cuts {
		if cuts[i].AtUS < 0 {
			return fault.New(fault.StageParse, fault.KindBadConfig, "negative cut at %dus", cuts[i].AtUS)
		}
		if i > 0 && cuts[i].AtUS == cuts[i-1].AtUS {
			return fault.New(fault.StageParse, fault.KindBadConfig, "duplicate cut at %dus", cuts[i].AtUS)
		}
		if tr := cuts[i].Transition; tr != nil {
			if err := tr.Validate(known); err != nil {
				return err
			}
		}
	}
	return nil
}

// SegmentID builds the stable, deterministic identifier for a segment. It
// depends only on representation, track and the aligned boundary, never on
// scheduling order.
func SegmentID(rep string, trackID int, startUS, endUS int64) string {
	return fmt.Sprintf("%s/t%d/%012d-%012d", rep, trackID, startUS, endUS)
}
