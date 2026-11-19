// Package model defines the integer-tick media timeline used by every stage of
// the archive recovery pipeline. All timestamps are represented as multiples of
// IntNs (1 nanosecond) so that conversions from arbitrary track clocks are exact
// and reproducible on both arm64 and amd64.
package model

import (
	"fmt"
	"sort"
)

// IntNs is the integer time base: one nanosecond per tick.
const IntNs int64 = 1

// Timeline is the unified, monotonic media timeline that every track maps onto.
// Dependants are expressed as integer nanosecond positions in Program order.
type Timeline struct {
	// Duration is the declared or derived program duration, in nanoseconds.
	Duration int64
	// Tracks holds one mapping per input track, keyed by TrackID.
	Tracks map[uint32]*TrackTimeline
	// Order lists TrackIDs in dependency (topological) order.
	Order []uint32
}

// TrackTimeline describes one recovered track in program time.
type TrackTimeline struct {
	TrackID   uint32   `json:"track_id"`
	Kind      string   `json:"kind"` // video, audio, index, unknown
	Timescale uint32   `json:"timescale"`
	PTS       []int64  `json:"-"` // program-time nanoseconds, monotonic
	RawPTS    []int64  `json:"-"` // source clock ticks, as read
	Duration  []int64  `json:"-"` // per-access-unit duration in ns, if known
	Keyframes []int    `json:"-"` // indexes into PTS that are sync points
	Reverse   int      `json:"reverse_steps"`
	Wrap      int      `json:"wrap_count"`
	DriftPPM  int64    `json:"drift_ppm"`
	Anchors   int      `json:"anchor_count"`
	Segments  [][2]int `json:"-"` // contiguous fitted spans [start,end) in sample indexes
}

// Kind constants.
const (
	KindVideo = "video"
	KindAudio = "audio"
	KindIndex = "index"
	KindOther = "unknown"
)

// ClockToNs converts ticks at the given timescale into integer nanoseconds,
// rounding half away from zero. The result is exact for all timescales that
// divide 1e9 and deterministic for the rest.
func ClockToNs(ticks int64, timescale uint32) int64 {
	if timescale == 0 {
		return 0
	}
	// Use 128-bit-free arithmetic by splitting: ticks*1e9 may overflow int64
	// for pathological inputs, so detect and saturate rather than wrap.
	const ns = int64(1_000_000_000)
	hi := ticks / int64(timescale)
	lo := ticks % int64(timescale)
	// Bound the per-sample value to a century of nanoseconds; anything larger
	// is corrupt input and cannot be represented in our timeline.
	if hi > 3_155_760_000 || hi < -3_155_760_000 {
		if hi > 0 {
			return ns * 3_155_760_000
		}
		return -ns * 3_155_760_000
	}
	out := hi*ns + (lo*ns+int64(timescale)/2)/int64(timescale)
	return out
}

// NsToClock is the inverse of ClockToNs for reporting purposes.
func NsToClock(ns int64, timescale uint32) int64 {
	if timescale == 0 {
		return 0
	}
	return (ns*int64(timescale) + 500_000_000) / 1_000_000_000
}

// MaxPTS returns the last program-time position across all tracks.
func (t *Timeline) MaxPTS() int64 {
	var m int64
	for _, tr := range t.Tracks {
		if n := len(tr.PTS); n > 0 && tr.PTS[n-1] > m {
			m = tr.PTS[n-1]
		}
	}
	return m
}

// SortedTracks returns tracks in deterministic order: dependency order first,
// then any remaining tracks by ascending ID.
func (t *Timeline) SortedTracks() []*TrackTimeline {
	out := make([]*TrackTimeline, 0, len(t.Tracks))
	seen := map[uint32]bool{}
	for _, id := range t.Order {
		if tr, ok := t.Tracks[id]; ok && !seen[id] {
			out = append(out, tr)
			seen[id] = true
		}
	}
	rest := make([]*TrackTimeline, 0)
	for id, tr := range t.Tracks {
		if !seen[id] {
			rest = append(rest, tr)
		}
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i].TrackID < rest[j].TrackID })
	return append(out, rest...)
}

// Validate checks the program-time invariants of every track. Non-monotonic
// program time is always a bug in a recovery stage, never expected input.
func (t *Timeline) Validate() error {
	for id, tr := range t.Tracks {
		for i := 1; i < len(tr.PTS); i++ {
			if tr.PTS[i] < tr.PTS[i-1] {
				return fmt.Errorf("track %d: program time regressed at sample %d (%d < %d)", id, i, tr.PTS[i], tr.PTS[i-1])
			}
		}
		for _, k := range tr.Keyframes {
			if k < 0 || k >= len(tr.PTS) {
				return fmt.Errorf("track %d: keyframe index %d out of range (%d samples)", id, k, len(tr.PTS))
			}
		}
	}
	return nil
}
