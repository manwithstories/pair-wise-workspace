// Package model defines the unified time base and the shared error model
// used by every subsystem (timecode, cue, pipe, hls, metrics).
package model

import "time"

// MediaMs is a media timestamp in milliseconds on the unified timeline.
// Media time 0 is the tour-recording epoch; all camera timecodes, cue
// times and segment boundaries are expressed on this single base so that
// cross-rendition drift cannot accumulate.
type MediaMs = int64

// Clock is the single time base shared by all subsystems.
type Clock struct {
	epoch time.Time // wall-clock instant of media time 0
	now   func() time.Time
}

// NewClock anchors media time 0 at epoch.
func NewClock(epoch time.Time) *Clock {
	return &Clock{epoch: epoch, now: time.Now}
}

// Now returns the current media time in milliseconds.
func (c *Clock) Now() MediaMs { return c.now().Sub(c.epoch).Milliseconds() }

// Since returns how many media milliseconds elapsed since m.
func (c *Clock) Since(m MediaMs) MediaMs { return c.Now() - m }

// At converts a media time back to wall clock.
func (c *Clock) At(m MediaMs) time.Time { return c.epoch.Add(time.Duration(m) * time.Millisecond) }
