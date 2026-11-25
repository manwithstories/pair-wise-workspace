// Package events aligns subtitles, chapters and in-band timed metadata to the
// shared nanosecond timeline and assigns them to half-open cut windows.
package events

import (
	"fmt"
	"sort"

	"fieldcut/internal/clock"
)

// Kind identifies the event track family.
type Kind string

const (
	KindSubtitle Kind = "subtitle"
	KindChapter  Kind = "chapter"
	KindID3      Kind = "id3"
	KindEMSG     Kind = "emsg"
	KindSEI      Kind = "sei"
)

// RawEvent retains an original clock value before normalization.
type RawEvent struct {
	ID           string
	GroupID      string // groups the same moment represented on several tracks
	TrackID      int
	Kind         Kind
	TimeBaseNum  int64
	TimeBaseDen  int64
	StartTicks   int64
	EndTicks     int64
	Label        string
	Payload      []byte
	RandomAccess bool
}

// Event is normalized on clock.Ns.
type Event struct {
	ID           string         `json:"id"`
	GroupID      string         `json:"group_id,omitempty"`
	TrackID      int            `json:"track_id"`
	Kind         Kind           `json:"kind"`
	Interval     clock.Interval `json:"interval"`
	Label        string         `json:"label,omitempty"`
	PayloadSHA   string         `json:"payload_sha256,omitempty"`
	RandomAccess bool           `json:"random_access,omitempty"`
	RawStart     int64          `json:"raw_start_ticks"`
	RawEnd       int64          `json:"raw_end_ticks"`
}

// Assignment describes one event's overlap with one segment. The global event
// remains unique; SegmentIDs records every segment that needs it.
type Assignment struct {
	EventID       string         `json:"event_id"`
	SegmentIndex  int            `json:"segment_index"`
	LocalInterval clock.Interval `json:"local_interval"`
	SegmentIDs    []int          `json:"segment_ids"`
}

// Align converts every raw rational timestamp and rejects invalid ranges.
// Valid events are returned even when another event is invalid so reports can
// locate the first error interval.
func Align(raw []RawEvent) ([]Event, []*clock.Error) {
	out := make([]Event, 0, len(raw))
	var errs []*clock.Error
	for i, r := range raw {
		id := r.ID
		if id == "" {
			id = fmt.Sprintf("event-%d", i)
		}
		num, den := r.TimeBaseNum, r.TimeBaseDen
		if num == 0 {
			num = 1
		}
		if den == 0 {
			den = 1_000_000_000
		}
		start, err := clock.TicksToNs(r.StartTicks, num, den)
		if err != nil {
			errs = append(errs, timestampError(err, r, "start"))
			continue
		}
		end, err := clock.TicksToNs(r.EndTicks, num, den)
		if err != nil {
			errs = append(errs, timestampError(err, r, "end"))
			continue
		}
		iv := clock.Interval{Start: start, End: end}
		if err := iv.Valid(); err != nil {
			ce, _ := clock.AsError(err)
			ce.TrackID = r.TrackID
			ce.OriginalTimestamp = firstRaw(r)
			ce.OriginalValue = fmt.Sprintf("event=%s start_ticks=%d end_ticks=%d timebase=%d/%d", id, r.StartTicks, r.EndTicks, num, den)
			errs = append(errs, ce)
			continue
		}
		var sha string
		if len(r.Payload) > 0 {
			sha = hashPayload(r.Payload)
		}
		out = append(out, Event{
			ID: id, GroupID: r.GroupID, TrackID: r.TrackID, Kind: r.Kind,
			Interval: iv, Label: r.Label, PayloadSHA: sha,
			RandomAccess: r.RandomAccess, RawStart: r.StartTicks, RawEnd: r.EndTicks,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Interval.Start != out[j].Interval.Start {
			return out[i].Interval.Start < out[j].Interval.Start
		}
		return out[i].ID < out[j].ID
	})
	return out, errs
}

func firstRaw(r RawEvent) int64 {
	if r.StartTicks != 0 {
		return r.StartTicks
	}
	return r.EndTicks
}

func timestampError(err error, r RawEvent, edge string) *clock.Error {
	ce, ok := clock.AsError(err)
	if !ok {
		ce = clock.Errorf(clock.CodeInvalidArgument, r.TrackID, "%s timestamp invalid: %v", edge, err)
	}
	ce.TrackID = r.TrackID
	ce.OriginalTimestamp = firstRaw(r)
	ce.OriginalValue = fmt.Sprintf("event_id=%s edge=%s start=%d end=%d", r.ID, edge, r.StartTicks, r.EndTicks)
	return ce
}

// CrossTrackSkew checks grouped events that represent the same source moment.
func CrossTrackSkew(events []Event, tolerance clock.Ns) []*clock.Error {
	groups := map[string][]Event{}
	for _, e := range events {
		if e.GroupID != "" {
			groups[e.GroupID] = append(groups[e.GroupID], e)
		}
	}
	ids := make([]string, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var errs []*clock.Error
	for _, id := range ids {
		es := groups[id]
		if len(es) < 2 {
			continue
		}
		minStart, maxStart := es[0].Interval.Start, es[0].Interval.Start
		for _, e := range es[1:] {
			minStart = clock.Min(minStart, e.Interval.Start)
			maxStart = clock.Max(maxStart, e.Interval.Start)
		}
		if maxStart-minStart > tolerance {
			first := es[0]
			errs = append(errs, &clock.Error{
				Code: clock.CodeBadInterval, TrackID: first.TrackID,
				StartNs: minStart, EndNs: maxStart,
				OriginalTimestamp: first.RawStart,
				OriginalValue:     fmt.Sprintf("group=%s skew_ns=%d tolerance_ns=%d", id, maxStart-minStart, tolerance),
				Message:           "cross-track event timestamps exceed alignment tolerance",
			})
		}
	}
	return errs
}

// Assign maps every event to all segments it intersects. Returned assignments
// are sorted and include a complete segment ID list per event.
func Assign(events []Event, segments []clock.Interval) ([]Assignment, error) {
	assignments := make([]Assignment, 0)
	for _, ev := range events {
		ids := make([]int, 0)
		for i, seg := range segments {
			if ev.Interval.End <= seg.Start || ev.Interval.Start >= seg.End {
				continue
			}
			ids = append(ids, i)
			local := clock.Interval{
				Start: maxNs(ev.Interval.Start, seg.Start) - seg.Start,
				End:   minNs(ev.Interval.End, seg.End) - seg.Start,
			}
			assignments = append(assignments, Assignment{
				EventID: ev.ID, SegmentIndex: i, LocalInterval: local, SegmentIDs: ids,
			})
		}
		if len(ids) == 0 && ev.Interval.End > 0 {
			return nil, clock.Errorf(clock.CodeCoverage, ev.TrackID,
				"event %s at %+v is outside segment coverage", ev.ID, ev.Interval)
		}
		// Fill final IDs into all assignment rows for this event.
		for j := range assignments {
			if assignments[j].EventID == ev.ID {
				assignments[j].SegmentIDs = append([]int(nil), ids...)
			}
		}
	}
	sort.SliceStable(assignments, func(i, j int) bool {
		if assignments[i].SegmentIndex != assignments[j].SegmentIndex {
			return assignments[i].SegmentIndex < assignments[j].SegmentIndex
		}
		return assignments[i].EventID < assignments[j].EventID
	})
	return assignments, nil
}

func maxNs(a, b clock.Ns) clock.Ns { return clock.Max(a, b) }
func minNs(a, b clock.Ns) clock.Ns { return clock.Min(a, b) }
