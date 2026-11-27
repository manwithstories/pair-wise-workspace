// Package validate enforces timeline invariants on the merged stream.
package validate

import (
	"fmt"

	"cueforge/internal/clock"
	"cueforge/internal/model"
)

// Checker is a stateful validator for the merged event stream.
type Checker struct {
	last    clock.Time
	hasLast bool
	count   int64
}

func NewChecker() *Checker { return &Checker{} }

// Check validates one event against the timeline invariants:
// legal track, non-negative duration, no timestamp overflow, no
// illegal wraparound, and the global record/event limits.
func (c *Checker) Check(ev model.Event) error {
	fail := func(msg string) error {
		return &model.TrackError{Track: ev.Track, Stage: model.StageValidate,
			Offset: ev.Offset, Time: ev.Start, Msg: msg}
	}
	if ev.Track < 0 || ev.Track >= model.MaxTracks {
		return fail(fmt.Sprintf("track index %d out of range", ev.Track))
	}
	if len(ev.Payload) > model.MaxRecordBytes {
		return fail("event payload exceeds 64 KiB record limit")
	}
	if err := clock.RequireNonNegative(ev.Dur); err != nil {
		return fail(err.Error())
	}
	if _, err := ev.Start.Add(ev.Dur); err != nil {
		return fail("timestamp overflow: " + err.Error())
	}
	if c.hasLast {
		if err := clock.CheckMonotonic(c.last, ev.Start); err != nil {
			return fail(err.Error())
		}
	}
	c.last, c.hasLast = ev.Start, true
	c.count++
	if c.count > model.MaxEvents {
		return fail(fmt.Sprintf("event limit %d exceeded", model.MaxEvents))
	}
	return nil
}

// Count returns the number of validated events.
func (c *Checker) Count() int64 { return c.count }
