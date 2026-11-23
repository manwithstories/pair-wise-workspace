// Package metadata normalizes out-of-band and in-band timed events
// (subtitles, chapters, ID3, emsg, SEI) onto the unified clock grid and
// measures clock drift against a reference timeline.
package metadata

import (
	"errors"
	"fmt"

	"mediascan/internal/clock"
	"mediascan/internal/fault"
)

// EventKind enumerates the supported timed-event sources.
type EventKind string

const (
	KindSubtitle EventKind = "subtitle"
	KindChapter  EventKind = "chapter"
	KindID3      EventKind = "id3"
	KindEmsg     EventKind = "emsg"
	KindSEI      EventKind = "sei"
)

// DriftDirection reports which way a track has moved.
type DriftDirection string

const (
	DriftNone   DriftDirection = "none"
	DriftAhead  DriftDirection = "ahead" // observed clock is ahead of reference
	DriftBehind DriftDirection = "behind"
)

// RawEvent is an event expressed in its source track timescale.
type RawEvent struct {
	Kind          EventKind
	ID            string
	TimeUnits     int64 // presentation time in track timescale units
	DurationUnits int64
	Payload       string
}

// Event is a RawEvent mapped onto the unified microsecond grid.
type Event struct {
	Kind          EventKind
	TrackID       uint32
	ID            string
	StartTicks    int64
	DurationTicks int64
	// RefTicks is the ideal reference position (zero drift). Populated by
	// the drift detector or fixture builder.
	RefTicks int64
	Payload  string
}

// DriftPoint is one measured drift sample.
type DriftPoint struct {
	EventIndex int
	TrackID    uint32
	// DeviationTicks is observedStart - referenceStart at this event, i.e.
	// the cumulative drift accumulated up to this point.
	DeviationTicks int64
	Direction      DriftDirection
}

// DriftReport summarizes a track's drift against the reference timeline.
type DriftReport struct {
	TrackID         uint32
	ThresholdTicks  int64
	MaxAbsDeviation int64
	FirstExceedance *DriftPoint
	Points          []DriftPoint
}

// ErrNoEvents is returned when normalization receives an empty event list.
var ErrNoEvents = errors.New("metadata: no events to normalize")

// NormalizeEvents maps source-timescale events onto the unified grid using
// exact integer rational conversion. Negative durations are rejected.
func NormalizeEvents(trackID uint32, timescale int64, raws []RawEvent) ([]Event, error) {
	if len(raws) == 0 {
		return nil, ErrNoEvents
	}
	rate, err := clock.Rate(timescale)
	if err != nil {
		return nil, fmt.Errorf("track %d: %w", trackID, err)
	}
	out := make([]Event, 0, len(raws))
	for i, raw := range raws {
		start, err := rate.Scale(raw.TimeUnits)
		if err != nil {
			return nil, fmt.Errorf("track %d event %d start: %w", trackID, i, err)
		}
		if raw.DurationUnits < 0 {
			return nil, fmt.Errorf("track %d event %d: %w", trackID, i, clock.ErrNegativeDuration)
		}
		dur, err := rate.Scale(raw.DurationUnits)
		if err != nil {
			return nil, fmt.Errorf("track %d event %d duration: %w", trackID, i, err)
		}
		out = append(out, Event{
			Kind: raw.Kind, TrackID: trackID, ID: raw.ID,
			StartTicks: start, DurationTicks: dur,
			RefTicks: start, Payload: raw.Payload,
		})
	}
	return out, nil
}

// WithReference overwrites each event's ideal reference position. The two
// slices must align by index.
func WithReference(events []Event, refTicks []int64) ([]Event, error) {
	if len(refTicks) != len(events) {
		return nil, fmt.Errorf("metadata: reference length %d != events %d",
			len(refTicks), len(events))
	}
	out := make([]Event, len(events))
	copy(out, events)
	for i := range out {
		out[i].RefTicks = refTicks[i]
	}
	return out, nil
}

// DetectDrift compares observed event positions against their reference
// positions. Deviation at each event is the cumulative clock offset
// (observed - reference). Once |deviation| crosses threshold a fault is
// raised naming the track, direction and cumulative deviation. VFR is
// handled naturally because every event carries its own exact timestamp.
func DetectDrift(events []Event, thresholdTicks int64) (DriftReport, []fault.Fault) {
	rep := DriftReport{ThresholdTicks: thresholdTicks}
	var faults []fault.Fault
	for i, ev := range events {
		dev := ev.StartTicks - ev.RefTicks
		dir := DriftNone
		switch {
		case dev > 0:
			dir = DriftAhead
		case dev < 0:
			dir = DriftBehind
		}
		abs := dev
		if abs < 0 {
			abs = -abs
		}
		if abs > rep.MaxAbsDeviation {
			rep.MaxAbsDeviation = abs
			if rep.TrackID == 0 {
				rep.TrackID = ev.TrackID
			}
		}
		point := DriftPoint{
			EventIndex: i, TrackID: ev.TrackID,
			DeviationTicks: dev, Direction: dir,
		}
		rep.Points = append(rep.Points, point)
		if thresholdTicks > 0 && abs > thresholdTicks && rep.FirstExceedance == nil {
			rep.FirstExceedance = &point
			faults = append(faults, fault.New(
				fault.KindClockDrift, fault.SeverityWarning, ev.TrackID,
				int64(i), -1, ev.StartTicks,
				fmt.Sprintf("clock drift %s by %d ticks (threshold %d) at event %s",
					dir, dev, thresholdTicks, ev.ID),
			))
		}
	}
	return rep, faults
}
