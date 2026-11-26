// Package timeline merges recovered packets into one integer-tick master
// timeline and estimates per-stream clock drift against supplier anchors.
// Source offsets and raw PTS values travel with every item so downstream
// stages can always trace a rendered frame back to the supplied material;
// drift correction is reported as a separate estimate, never written back
// onto the source values.
package timeline

import (
	"fmt"

	"bq/internal/clock"
	"bq/internal/manifest"
	"bq/internal/model"
)

// Item is one packet placed on the unified master timeline.
type Item struct {
	StreamID     int
	Seq          int64
	GroupID      int64
	Kind         string
	SourceOffset int64
	RawPTS       int64 // supplier timestamp, retained verbatim
	UnwrappedPTS int64 // monotonic timestamp in source ticks
	MasterPTS    int64 // normalized master-tick position
	DurationTick int64
	Keyframe     bool
	WrapCount    int
}

// Timeline holds every stream's normalized items in packet order.
type Timeline struct {
	MasterHz int64
	Streams  map[int][]Item
	// Edits mirrors declared edit lists in master ticks for the transform.
	Edits map[int][]EditSegment
	// Errors are non-fatal per-stream records; fatal input is caught earlier.
	Errors []model.Error
}

// EditSegment is an edit list entry converted to master ticks.
type EditSegment struct {
	StartMaster int64
	EndMaster   int64
	StartSource int64
	EndSource   int64
	InBounds    bool
}

// Compose normalizes recovered media packets. wrapEvents is the per-stream
// expansion state already advanced by the transport/clock flow; callers may
// pass a fresh manifest-driven expansion via NewWrapFromManifest.
func Compose(m *manifest.Manifest, perStream map[int][]manifest.Packet,
	expanders map[int]*clock.WrapExpander, wrapEvents map[int][]clock.WrapEvent) (*Timeline, []model.Error) {

	tl := &Timeline{
		MasterHz: clock.MasterHz,
		Streams:  make(map[int][]Item),
		Edits:    make(map[int][]EditSegment),
	}
	var errs []model.Error

	for i := range m.Streams {
		s := &m.Streams[i]
		rate, err := clock.NewRate(s.TimebaseNum, s.TimebaseDen)
		if err != nil {
			errs = append(errs, model.NewError(model.StageTimeline, s.ID, model.ErrCorruptManifest,
				model.SeverityFatal, err.Error(), -1, 0, false))
			return nil, errs
		}
		exp := expanders[s.ID]
		streamEvents := wrapEvents[s.ID]
		eventByOffset := indexWrapEvents(streamEvents)

		items := make([]Item, 0, len(perStream[s.ID]))
		for _, p := range perStream[s.ID] {
			if p.Kind != manifest.KindMedia {
				continue
			}
			unwrapped := p.RawPTS
			var wraps int
			if exp != nil {
				// Re-expansion is only used when the caller did not pre-expand;
				// normally recovered packets arrive with expander state shared.
				full, _, err := exp.Expand(p.RawPTS, p.SourceOffset)
				if err != nil {
					errs = append(errs, model.NewError(model.StageClock, s.ID, model.ErrIntegerOverflow,
						model.SeverityFatal, err.Error(), p.SourceOffset, p.RawPTS, true))
					return nil, errs
				}
				unwrapped = full
				wraps = exp.Wraps()
			}
			if ev, ok := eventByOffset[p.SourceOffset]; ok {
				unwrapped = ev.UnwrappedPTS
				wraps = ev.WrapCount
			}
			master, err := rate.ToMaster(unwrapped)
			if err != nil {
				errs = append(errs, model.NewError(model.StageClock, s.ID, model.ErrIntegerOverflow,
					model.SeverityFatal, fmt.Sprintf("normalize seq=%d: %v", p.Seq, err),
					p.SourceOffset, p.RawPTS, true))
				return nil, errs
			}
			items = append(items, Item{
				StreamID:     s.ID,
				Seq:          p.Seq,
				GroupID:      p.GroupID,
				Kind:         p.Kind,
				SourceOffset: p.SourceOffset,
				RawPTS:       p.RawPTS,
				UnwrappedPTS: unwrapped,
				MasterPTS:    master,
				DurationTick: p.DurationTick,
				Keyframe:     p.Keyframe,
				WrapCount:    wraps,
			})
		}
		tl.Streams[s.ID] = items
		tl.Edits[s.ID] = convertEdits(s, rate, &errs)
	}
	tl.Errors = errs
	return tl, errs
}

func indexWrapEvents(events []clock.WrapEvent) map[int64]clock.WrapEvent {
	out := make(map[int64]clock.WrapEvent, len(events))
	for _, ev := range events {
		out[ev.SourceOffset] = ev
	}
	return out
}

func convertEdits(s *manifest.Stream, rate clock.Rate, errs *[]model.Error) []EditSegment {
	out := make([]EditSegment, 0, len(s.EditList))
	for _, ed := range s.EditList {
		startM, err1 := rate.ToMaster(ed.StartPTS)
		endM, err2 := rate.ToMaster(ed.EndPTS)
		if err1 != nil || err2 != nil {
			err := err1
			if err == nil {
				err = err2
			}
			*errs = append(*errs, model.NewError(model.StageTimeline, s.ID, model.ErrIntegerOverflow,
				model.SeverityFatal, err.Error(), -1, ed.StartPTS, true))
			return nil
		}
		out = append(out, EditSegment{
			StartMaster: startM, EndMaster: endM,
			StartSource: ed.StartPTS, EndSource: ed.EndPTS, InBounds: true,
		})
	}
	return out
}

// ItemsAtOrBefore returns the latest timeline item at or before masterPTS,
// used by the renderer's sparse-keyframe forward location logic.
func ItemsAtOrBefore(items []Item, masterPTS int64) (Item, bool) {
	var found Item
	ok := false
	for _, it := range items {
		if it.MasterPTS <= masterPTS {
			found = it
			ok = true
		} else {
			break
		}
	}
	return found, ok
}

// NextKeyframeAtOrAfter locates the last keyframe at or before masterPTS.
// Sparse GOPs mean the decoder must seek backward to this frame and decode
// forward; the returned item carries the actual landing tick so the render
// stage can verify the seek landed where the timeline predicted.
func NextKeyframeAtOrAfter(items []Item, masterPTS int64) (Item, bool) {
	var kf Item
	found := false
	for _, it := range items {
		if it.MasterPTS > masterPTS {
			break
		}
		if it.Keyframe {
			kf = it
			found = true
		}
	}
	return kf, found
}
