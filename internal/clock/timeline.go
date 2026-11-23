// Package clock provides the shared media time base used by every stage of
// the pipeline. Media samples are expressed in per-track timescale ticks in
// their container; all cross-representation reasoning (damage mapping,
// boundary checks, RTP sync) happens on a single microsecond timeline.
package clock

import (
	"errors"
	"math"
)

// Microseconds is the unified media timeline unit. Integer microseconds keep
// the summaries reproducible byte-for-byte for a given input.
const Microseconds int64 = 1

// UsPerMS is the number of unified ticks in one millisecond.
const UsPerMS int64 = 1000

var (
	// ErrNoTimescale is returned when a track has no usable timescale.
	ErrNoTimescale = errors.New("clock: track timescale is zero")
	// ErrUnknownTrack is returned for a track that is not on the timeline.
	ErrUnknownTrack = errors.New("clock: unknown track")
	// ErrOverflow is returned when a tick value cannot be represented safely
	// on the microsecond timeline.
	ErrOverflow = errors.New("clock: tick value overflows timeline conversion")
)

// TrackClock maps one track's timescale onto the unified timeline. OriginUs is
// the unified instant corresponding to OriginTicks in the track's timescale.
type TrackClock struct {
	TrackID     uint32 `json:"track_id"`
	Timescale   uint32 `json:"timescale"`
	OriginTicks uint64 `json:"origin_ticks"`
	OriginUs    int64  `json:"origin_us"`
}

// ToUnifiedUs converts a tick position on this track to the shared timeline.
// It uses round-half-away-from-zero on the microsecond conversion.
func (tc TrackClock) ToUnifiedUs(ticks uint64) (int64, error) {
	if tc.Timescale == 0 {
		return 0, ErrNoTimescale
	}
	if ticks > math.MaxInt64 || tc.OriginTicks > math.MaxInt64 {
		return 0, ErrOverflow
	}
	delta := int64(ticks) - int64(tc.OriginTicks)
	if delta > 0 && uint64(delta) > uint64(math.MaxInt64/1_000_000) {
		return 0, ErrOverflow
	}
	if delta < 0 && delta < -math.MaxInt64/1_000_000 {
		return 0, ErrOverflow
	}
	us, err := scale(delta, tc.Timescale)
	if err != nil {
		return 0, err
	}
	if (us > 0 && tc.OriginUs > math.MaxInt64-us) || (us < 0 && tc.OriginUs < math.MinInt64-us) {
		return 0, ErrOverflow
	}
	return tc.OriginUs + us, nil
}

// safeDelta returns us-OriginUs without wrapping.
func safeDelta(us, origin int64) (int64, bool) {
	if origin > 0 && us < math.MinInt64+origin {
		return 0, false
	}
	if origin < 0 && us > math.MaxInt64+origin {
		return 0, false
	}
	return us - origin, true
}

// FromUnifiedUs is the inverse of ToUnifiedUs.
func (tc TrackClock) FromUnifiedUs(us int64) (uint64, error) {
	if tc.Timescale == 0 {
		return 0, ErrNoTimescale
	}
	delta, ok := safeDelta(us, tc.OriginUs)
	if !ok {
		return 0, ErrOverflow
	}
	if delta > math.MaxInt64/int64(tc.Timescale) || delta < math.MinInt64/int64(tc.Timescale) {
		return 0, ErrOverflow
	}
	ticks := scaleUs(delta, tc.Timescale)
	if ticks < 0 {
		if uint64(-(ticks+1))+1 > tc.OriginTicks {
			return 0, ErrOverflow
		}
		return tc.OriginTicks - uint64(-ticks), nil
	}
	if uint64(ticks) > math.MaxUint64-tc.OriginTicks {
		return 0, ErrOverflow
	}
	return tc.OriginTicks + uint64(ticks), nil
}

func scale(deltaTicks int64, timescale uint32) (int64, error) {
	ts := int64(timescale)
	if deltaTicks > math.MaxInt64/1_000_000 || deltaTicks < -math.MaxInt64/1_000_000 {
		return 0, ErrOverflow
	}
	prod := deltaTicks * 1_000_000
	q := prod / ts
	r := prod % ts
	if r*2 >= ts {
		q++
	} else if r*2 <= -ts {
		q--
	}
	return q, nil
}

func scaleUs(deltaUs int64, timescale uint32) int64 {
	prod := deltaUs * int64(timescale)
	q := prod / 1_000_000
	r := prod % 1_000_000
	if r*2 >= 1_000_000 {
		q++
	} else if r*2 <= -1_000_000 {
		q--
	}
	return q
}

// Timeline owns the track clocks for one representation set. Reads are safe
// for concurrent use; registration is expected before fan-out begins.
type Timeline struct {
	Tracks map[uint32]TrackClock
}

// NewTimeline returns an empty timeline.
func NewTimeline() *Timeline {
	return &Timeline{Tracks: make(map[uint32]TrackClock)}
}

// Add registers a track clock.
func (tl *Timeline) Add(tc TrackClock) { tl.Tracks[tc.TrackID] = tc }

// Track returns the clock for trackID.
func (tl *Timeline) Track(trackID uint32) (TrackClock, bool) {
	tc, ok := tl.Tracks[trackID]
	return tc, ok
}

// ToUs converts ticks on trackID to the unified timeline.
func (tl *Timeline) ToUs(trackID uint32, ticks uint64) (int64, error) {
	tc, ok := tl.Track(trackID)
	if !ok {
		return 0, ErrUnknownTrack
	}
	return tc.ToUnifiedUs(ticks)
}
