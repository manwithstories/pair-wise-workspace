// Package timeline owns the shared timeline of audio tracks and video
// keyframe anchors. Timestamps are validated here before any sample is
// processed so that ordering faults cannot reach the output stage.
package timeline

import (
	"fmt"
	"sort"
	"time"

	"avpipe/internal/media"
)

// Kind distinguishes audio tracks (which carry audio blocks) from video
// tracks (which only carry keyframe anchor timestamps).
type Kind string

const (
	// Audio carries PCM blocks.
	Audio Kind = "audio"
	// Video carries keyframe anchors only; its samples are never decoded.
	Video Kind = "video"
)

// Anchor is a video keyframe timestamp on the shared timeline.
type Anchor struct {
	At    time.Duration
	Label string
}

// Track is a validated timeline track. Audio tracks declare their channel
// layout; video tracks only declare anchors.
type Track struct {
	ID       string
	Kind     Kind
	Rate     media.Rate
	Channels int
	Labels   []string
	Anchors  []Anchor
}

type trackState struct {
	track      Track
	anchors    []int64
	lastEnd    int64
	started    bool
	missing    int64
	seenSeq    map[uint64]uint64
	lastSeq    uint64
	hasSeq     bool
	duplicates uint64
}

// Timeline is the shared clock plus every registered track.
type Timeline struct {
	clock    media.Clock
	order    []string
	tracks   map[string]*trackState
	anchors  []int64
	final    bool
	missing  int64
	accepted uint64
}

// New creates an empty timeline at the shared rate.
func New(rate media.Rate) (*Timeline, error) {
	clock, err := media.NewClock(rate)
	if err != nil {
		return nil, media.NewError(media.PhaseTimeline, media.ExitUsage, "", 0, false, "new", err)
	}
	return &Timeline{clock: clock, tracks: make(map[string]*trackState)}, nil
}

// Clock returns the shared timeline clock.
func (t *Timeline) Clock() media.Clock { return t.clock }

// Add registers a track and validates its metadata immediately.
func (t *Timeline) Add(track Track) error {
	if t.final {
		return media.NewError(media.PhaseTimeline, media.ExitUsage, track.ID, 0, false, "add",
			fmt.Errorf("timeline already finalized"))
	}
	if track.ID == "" {
		return media.NewError(media.PhaseTimeline, media.ExitUsage, track.ID, 0, false, "add",
			fmt.Errorf("track id is empty"))
	}
	if _, exists := t.tracks[track.ID]; exists {
		return media.NewError(media.PhaseTimeline, media.ExitUsage, track.ID, 0, false, "add",
			fmt.Errorf("duplicate track id"))
	}
	switch track.Kind {
	case Audio:
		if err := t.validateAudio(track); err != nil {
			return err
		}
	case Video:
		if err := t.validateVideo(track); err != nil {
			return err
		}
	default:
		return media.NewError(media.PhaseTimeline, media.ExitUsage, track.ID, 0, false, "add",
			fmt.Errorf("unknown track kind %q", track.Kind))
	}
	st := &trackState{track: track, seenSeq: make(map[uint64]uint64)}
	if track.Kind == Video {
		for _, a := range track.Anchors {
			if a.At < 0 {
				return media.NewError(media.PhaseTimeline, media.ExitUsage, track.ID, 0, false, "anchor",
					fmt.Errorf("negative anchor %v", a.At))
			}
			f := t.clock.Frames(a.At)
			if n := len(st.anchors); n > 0 && f <= st.anchors[n-1] {
				return media.NewError(media.PhaseTimeline, media.ExitUsage, track.ID, 0, false, "anchor",
					fmt.Errorf("anchors must be strictly increasing at %v", a.At))
			}
			st.anchors = append(st.anchors, f)
		}
		t.anchors = append(t.anchors, st.anchors...)
	}
	t.tracks[track.ID] = st
	t.order = append(t.order, track.ID)
	return nil
}

func (t *Timeline) validateAudio(track Track) error {
	if !track.Rate.Valid() {
		return media.NewError(media.PhaseTimeline, media.ExitUsage, track.ID, 0, false, "add",
			fmt.Errorf("invalid audio rate %s", track.Rate.String()))
	}
	if track.Channels <= 0 {
		return media.NewError(media.PhaseTimeline, media.ExitUsage, track.ID, 0, false, "add",
			fmt.Errorf("audio track needs positive channels"))
	}
	if len(track.Labels) != track.Channels {
		return media.NewError(media.PhaseTimeline, media.ExitUsage, track.ID, 0, false, "add",
			fmt.Errorf("%d labels for %d channels", len(track.Labels), track.Channels))
	}
	for _, label := range track.Labels {
		if label == "" {
			return media.NewError(media.PhaseTimeline, media.ExitUsage, track.ID, 0, false, "add",
				fmt.Errorf("empty channel label"))
		}
	}
	return nil
}

func (t *Timeline) validateVideo(track Track) error {
	if track.Channels != 0 || len(track.Labels) != 0 {
		return media.NewError(media.PhaseTimeline, media.ExitUsage, track.ID, 0, false, "add",
			fmt.Errorf("video tracks carry anchors only"))
	}
	if len(track.Anchors) == 0 {
		return media.NewError(media.PhaseTimeline, media.ExitUsage, track.ID, 0, false, "add",
			fmt.Errorf("video track needs at least one anchor"))
	}
	return nil
}

// Finalize locks registration and requires at least one audio track.
func (t *Timeline) Finalize() error {
	if t.final {
		return nil
	}
	if len(t.order) == 0 {
		return media.NewError(media.PhaseTimeline, media.ExitUsage, "", 0, false, "finalize",
			fmt.Errorf("timeline has no tracks"))
	}
	audio := 0
	for _, id := range t.order {
		if t.tracks[id].track.Kind == Audio {
			audio++
		}
	}
	if audio == 0 {
		return media.NewError(media.PhaseTimeline, media.ExitUsage, "", 0, false, "finalize",
			fmt.Errorf("timeline has no audio track"))
	}
	sort.Slice(t.anchors, func(i, j int) bool { return t.anchors[i] < t.anchors[j] })
	t.final = true
	return nil
}

// Track returns a copy of the registered track metadata.
func (t *Timeline) Track(id string) (Track, bool) {
	st, ok := t.tracks[id]
	if !ok {
		return Track{}, false
	}
	track := st.track
	track.Labels = append([]string(nil), track.Labels...)
	track.Anchors = append([]Anchor(nil), track.Anchors...)
	return track, true
}

// IDs lists track ids in registration order.
func (t *Timeline) IDs() []string { return append([]string(nil), t.order...) }

// AudioIDs lists audio track ids in registration order.
func (t *Timeline) AudioIDs() []string {
	var out []string
	for _, id := range t.order {
		if t.tracks[id].track.Kind == Audio {
			out = append(out, id)
		}
	}
	return out
}

// Keyframes returns every video keyframe, in shared-timeline frames.
func (t *Timeline) Keyframes() []int64 { return append([]int64(nil), t.anchors...) }

// AnchorsIn counts anchors in the half-open shared-frame range [start, end).
func (t *Timeline) AnchorsIn(start, end int64) int {
	n := 0
	for _, a := range t.anchors {
		if a >= start && a < end {
			n++
		}
	}
	return n
}

// Advance validates that a block starts at or after the previous block's end.
// Overlap or reordering is rejected; a gap is reported so the runner can
// insert silence and record the missing frame count.
func (t *Timeline) Advance(id string, start int64, frames int) (int64, error) {
	st, ok := t.tracks[id]
	if !ok {
		return 0, media.NewError(media.PhaseTimeline, media.ExitUsage, id, 0, false, "advance",
			fmt.Errorf("unknown track"))
	}
	if start < 0 || frames < 0 {
		return 0, media.NewError(media.PhaseInput, media.ExitInput, id, 0, false, "advance",
			fmt.Errorf("negative position start=%d frames=%d", start, frames))
	}
	var gap int64
	if st.started {
		if start < st.lastEnd {
			return 0, media.NewError(media.PhaseInput, media.ExitInput, id, 0, false, "advance",
				fmt.Errorf("timeline reorder: start %d precedes end %d", start, st.lastEnd))
		}
		gap = start - st.lastEnd
		if gap > 0 {
			st.missing += gap
			t.missing += gap
		}
	}
	st.started = true
	st.lastEnd = start + int64(frames)
	return gap, nil
}

// Accept records a block sequence number and hash. A repeat of a sequence
// number with the same hash is a duplicate to be dropped; the same number
// with a different hash is a conflict.
func (t *Timeline) Accept(id string, seq uint64, hash uint64) (bool, error) {
	st, ok := t.tracks[id]
	if !ok {
		return false, media.NewError(media.PhaseTimeline, media.ExitUsage, id, seq, false, "accept",
			fmt.Errorf("unknown track"))
	}
	if st.hasSeq && seq < st.lastSeq {
		return false, media.NewError(media.PhaseInput, media.ExitInput, id, seq, false, "accept",
			fmt.Errorf("sequence rewind: %d after %d", seq, st.lastSeq))
	}
	if prev, seen := st.seenSeq[seq]; seen {
		if prev != hash {
			return false, media.NewError(media.PhaseInput, media.ExitInput, id, seq, false, "accept",
				fmt.Errorf("%w: seq %d hash %016x != %016x", media.ErrConflict, seq, hash, prev))
		}
		st.duplicates++
		return true, nil
	}
	st.seenSeq[seq] = hash
	st.lastSeq = seq
	st.hasSeq = true
	t.accepted++
	return false, nil
}

// Stats summarizes timeline validation activity.
type Stats struct {
	Tracks        int
	Anchors       int
	MissingFrames int64
	Duplicates    uint64
	Accepted      uint64
}

// Stats returns the accumulated timeline counters.
func (t *Timeline) Stats() Stats {
	var dup uint64
	for _, st := range t.tracks {
		dup += st.duplicates
	}
	return Stats{
		Tracks:        len(t.order),
		Anchors:       len(t.anchors),
		MissingFrames: t.missing,
		Duplicates:    dup,
		Accepted:      t.accepted,
	}
}
