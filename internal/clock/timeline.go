// Package clock converts every track's native sample clock onto one
// millisecond timeline and validates timebase parameters.
package clock

import (
	"fmt"
	"math"

	"mediacheck/internal/model"
)

// MaxTimelineMs bounds the accepted timeline. Values beyond it are
// treated as integer-overflow indicators rather than accepted.
const MaxTimelineMs int64 = 24 * 60 * 60 * 1000 // 24 h

// rtpMod is the modulus of a 32-bit RTP timestamp.
const rtpMod int64 = 1 << 32

// Timeline owns the mapping from per-track tick spaces to unified ms.
type Timeline struct {
	tracks map[string]*TrackClock
	faults []model.Fault
	nextID int
}

// TrackClock is the conversion state for one track.
type TrackClock struct {
	Track      string
	Timescale  uint32 // ticks per second; 0 means invalid
	rtpClock   uint32 // RTP clock rate in Hz when the track is RTP-fed
	rtpBase    int64  // first RTP timestamp, unwrapped
	rtpHigh    int64  // highest unwrapped (relative) timestamp observed
	rtpWrapped uint64 // accumulated wrap count (32-bit RTP timestamps)
	haveBase   bool
}

// New returns an empty timeline.
func New() *Timeline {
	return &Timeline{tracks: map[string]*TrackClock{}, nextID: 1}
}

// AddTrack registers a track. A zero timescale is recorded and a fault
// is raised; the caller may keep feeding the track but its samples
// cannot be placed until a valid timescale appears.
func (t *Timeline) AddTrack(track string, timescale uint32) *TrackClock {
	c := &TrackClock{Track: track, Timescale: timescale}
	t.tracks[track] = c
	if timescale == 0 {
		t.raise(model.KindInvalidTimebase, track, 0, 0, "zero timescale")
	}
	return c
}

// Get returns the registered track clock, or nil.
func (t *Timeline) Get(track string) *TrackClock { return t.tracks[track] }

// TicksToMs converts a native tick offset to unified milliseconds.
// Negative durations and overflow are reported as faults.
func (t *Timeline) TicksToMs(track string, ticks int64) (int64, bool) {
	c, ok := t.tracks[track]
	if !ok {
		t.raise(model.KindInvalidTimebase, track, 0, 0, "unregistered track")
		return 0, false
	}
	if c.Timescale == 0 {
		t.raise(model.KindInvalidTimebase, track, 0, 0, "conversion with zero timescale")
		return 0, false
	}
	if ticks < 0 {
		t.raise(model.KindInvalidTimebase, track, ticks, 0, "negative duration")
		return 0, false
	}
	if ticks > math.MaxInt64/1000 {
		t.raise(model.KindInvalidTimebase, track, ticks, 0, "timestamp overflow")
		return MaxTimelineMs, false
	}
	ms := ticks * 1000 / int64(c.Timescale)
	if ms > MaxTimelineMs {
		t.raise(model.KindInvalidTimebase, track, ticks, ms, "timestamp beyond 24h")
		return MaxTimelineMs, false
	}
	return ms, true
}

// RTPToMs unwraps a 32-bit RTP timestamp and maps it to the unified
// timeline. The first packet anchors the origin.
func (t *Timeline) RTPToMs(track string, ts uint32) (int64, bool) {
	c, ok := t.tracks[track]
	if !ok || c.rtpClock == 0 {
		t.raise(model.KindInvalidTimebase, track, 0, 0, "RTP track without clock rate")
		return 0, false
	}
	if !c.haveBase {
		c.rtpBase = int64(ts)
		c.haveBase = true
	}
	// Nearest-of-two unwrapping: a raw value maps to either the same
	// cycle or the next wrapped cycle, whichever is closer to the
	// highest unwrapped point. Late packets resolve into the past
	// instead of being mistaken for a new wrap.
	candidate := int64(ts) + int64(c.rtpWrapped)*rtpMod - c.rtpBase
	next := candidate + rtpMod
	if next-c.rtpHigh < c.rtpHigh-candidate {
		c.rtpWrapped++
		candidate = next
	}
	if candidate > c.rtpHigh {
		c.rtpHigh = candidate
	}
	if candidate < 0 {
		return 0, false // pre-origin packet: do not invent time
	}
	ms := candidate * 1000 / int64(c.rtpClock)
	if ms > MaxTimelineMs {
		t.raise(model.KindInvalidTimebase, track, candidate, ms, "RTP timestamp overflow")
		return MaxTimelineMs, false
	}
	return ms, true
}

// SetRTPClock marks a track as RTP-fed with the given clock rate.
func (t *Timeline) SetRTPClock(track string, rate uint32) {
	if c, ok := t.tracks[track]; ok && rate != 0 {
		c.rtpClock = rate
	}
}

// Drift reports the absolute cross-track drift in milliseconds.
func Drift(aMs, bMs int64) int64 {
	d := aMs - bMs
	if d < 0 {
		return -d
	}
	return d
}

// Faults returns all faults raised during conversion, in insertion order.
func (t *Timeline) Faults() []model.Fault { return t.faults }

func (t *Timeline) raise(k model.Kind, track string, start, end int64, detail string) {
	t.faults = append(t.faults, model.Fault{
		ID:       t.nextID,
		Kind:     k,
		Severity: model.SeverityError,
		Track:    track,
		StartMs:  start,
		EndMs:    end,
		Detail:   detail,
		Recovery: model.RecPending,
	})
	t.nextID++
}

// Describe renders a human-readable conversion summary for diagnostics.
func (c *TrackClock) Describe() string {
	return fmt.Sprintf("track=%s timescale=%d rtpRate=%d", c.Track, c.Timescale, c.rtpClock)
}
