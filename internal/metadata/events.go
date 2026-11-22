// Package metadata aligns subtitles, chapter marks and timed metadata
// carriers (ID3, emsg, SEI) onto the unified A/V timeline. Events at
// the same instant use a stable total order, and events that cannot be
// anchored to any track are retained as explicit faults.
package metadata

import (
	"fmt"
	"sort"

	"mediacheck/internal/model"
)

// Kind enumerates the supported timed-event carriers.
type Kind string

const (
	KindSubtitle Kind = "subtitle"
	KindChapter  Kind = "chapter"
	KindID3      Kind = "id3"
	KindEMSG     Kind = "emsg"
	KindSEI      Kind = "sei"
)

// rank defines the stable tie-break order for equal timestamps.
var rank = map[Kind]int{
	KindChapter: 0, KindEMSG: 1, KindID3: 2, KindSEI: 3, KindSubtitle: 4,
}

// Event is one timed metadata item after timeline placement.
type Event struct {
	Kind    Kind   `json:"kind"`
	Track   string `json:"track"` // originating track, if known
	StartMs int64  `json:"start_ms"`
	EndMs   int64  `json:"end_ms"`
	Payload string `json:"payload"`
	Seq     int    `json:"seq"` // arrival order, final tie-break
}

// AlignmentReport holds ordered events and un-anchored faults.
type AlignmentReport struct {
	Events []Event       `json:"events"`
	Faults []model.Fault `json:"faults"`
	nextID int
}

// TrackWindow is the valid time span of one media track. Events whose
// time falls outside every track window are un-anchored.
type TrackWindow struct {
	Track   string
	StartMs int64
	EndMs   int64
}

// AlignOptions configures alignment.
type AlignOptions struct {
	// MaxLeadMs rejects events this far before the earliest track start.
	MaxLeadMs int64
	// MaxLagMs rejects events this far past the latest track end.
	MaxLagMs int64
}

// DefaultOptions tolerates a small encoding lead/lag.
func DefaultOptions() AlignOptions {
	return AlignOptions{MaxLeadMs: 100, MaxLagMs: 1000}
}

// Align places every event on the timeline, sorts them deterministically
// and turns un-anchored events into orphan_event faults that are kept
// (never silently dropped).
func Align(events []Event, windows []TrackWindow, opts AlignOptions) AlignmentReport {
	rep := AlignmentReport{Events: make([]Event, 0, len(events)), nextID: 1}
	var lo, hi int64
	if len(windows) > 0 {
		lo, hi = windows[0].StartMs, windows[0].EndMs
	}
	for _, w := range windows {
		if w.StartMs < lo {
			lo = w.StartMs
		}
		if w.EndMs > hi {
			hi = w.EndMs
		}
	}
	for _, ev := range events {
		if !anchorable(ev, windows, lo, hi, opts) {
			rep.Faults = append(rep.Faults, model.Fault{
				ID: rep.nextID, Kind: model.KindOrphanEvent, Severity: model.SeverityError,
				Track: ev.Track, StartMs: ev.StartMs, EndMs: ev.EndMs,
				Detail:   fmt.Sprintf("%s event outside all track windows", ev.Kind),
				Recovery: model.RecPending,
			})
			rep.nextID++
			continue
		}
		rep.Events = append(rep.Events, ev)
	}
	sort.SliceStable(rep.Events, func(i, j int) bool {
		a, b := rep.Events[i], rep.Events[j]
		if a.StartMs != b.StartMs {
			return a.StartMs < b.StartMs
		}
		if rank[a.Kind] != rank[b.Kind] {
			return rank[a.Kind] < rank[b.Kind]
		}
		if a.Track != b.Track {
			return a.Track < b.Track
		}
		return a.Seq < b.Seq
	})
	return rep
}

func anchorable(ev Event, windows []TrackWindow, lo, hi int64, opts AlignOptions) bool {
	if len(windows) == 0 {
		return false
	}
	for _, w := range windows {
		if ev.StartMs >= w.StartMs && ev.StartMs <= w.EndMs {
			return true
		}
	}
	// Allow a small global lead/lag margin before declaring orphan.
	return ev.StartMs >= lo-opts.MaxLeadMs && ev.StartMs <= hi+opts.MaxLagMs
}
