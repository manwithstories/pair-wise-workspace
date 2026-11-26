// Package repair rebuilds monotonic DTS, applies edit lists and maps
// presentation times onto the shared 90 kHz clock. All conversions are
// integer-only absolute-position conversions (no float accumulation).
package repair

import (
	"fmt"

	"mediaroute/internal/clock"
	"mediaroute/internal/model"
	"mediaroute/internal/mp4"
)

// RepairTrack converts a scanned raw track into a repaired track on the
// shared clock. Detected faults are recorded on the track; the repair
// itself is always best-effort so other representations keep flowing.
func RepairTrack(rt *mp4.RawTrack, id model.RepresentationID) (*model.Track, error) {
	tl := clock.Timeline{Timescale: rt.Timescale}
	if !tl.Valid() {
		return nil, model.Newf(model.CodeUnrecoverable, "repair "+id.String(),
			"invalid timescale %d", rt.Timescale)
	}
	t := &model.Track{
		ID:        id,
		Timescale: rt.Timescale,
		Samples:   rt.Expand(),
		Gaps:      rt.Gaps,
		Faults:    append([]model.Fault(nil), rt.Faults...),
	}
	repairDTS(t)
	offset := applyEditList(t, rt)
	mapClock(t, tl, offset)
	return t, nil
}

// repairDTS enforces strictly increasing DTS. Regressions (zero or
// backwards deltas from broken stts) are bumped forward by one tick and
// recorded once per track. PTS keeps its original offset from DTS.
func repairDTS(t *model.Track) {
	reported := false
	for i := 1; i < len(t.Samples); i++ {
		prev, cur := &t.Samples[i-1], &t.Samples[i]
		if cur.DTS <= prev.DTS {
			if !reported {
				t.Faults = append(t.Faults, model.Fault{
					Code:   model.FaultDTSRegression,
					Offset: uint64(cur.Index),
					Detail: fmt.Sprintf("DTS %d not after %d; bumped to monotonic",
						cur.DTS, prev.DTS),
				})
				reported = true
			}
			shift := prev.DTS + 1 - cur.DTS
			cur.DTS += shift
			cur.PTS += shift
		}
	}
}

// applyEditList resolves the presentation offset from the edit list.
// A media time beyond the media duration is out of range: the edit is
// ignored (offset 0) and recorded as a recovered fault. A valid non-zero
// media time is the non-zero start timestamp that gets rebased to zero.
func applyEditList(t *model.Track, rt *mp4.RawTrack) int64 {
	if !rt.HasElst {
		return 0
	}
	for _, e := range rt.Elst {
		mt := e.MediaTimeV1
		if rt.ElstVersion == 0 {
			mt = int64(e.MediaTimeV0)
		}
		if mt < 0 { // empty edit: dwell, no media mapping
			continue
		}
		if uint64(mt) > rt.Duration {
			t.Faults = append(t.Faults, model.Fault{
				Code:   model.FaultElstRange,
				Offset: uint64(mt),
				Detail: fmt.Sprintf("edit media time %d beyond duration %d; edit ignored",
					mt, rt.Duration),
			})
			return 0
		}
		return mt
	}
	return 0
}

// mapClock converts presentation times to the shared 90 kHz clock,
// rebasing the non-zero start offset to zero. Samples that fall before
// the edit-list offset are not presented and get dropped.
func mapClock(t *model.Track, tl clock.Timeline, mediaOffset int64) {
	kept := t.Samples[:0]
	for _, s := range t.Samples {
		clk := tl.ToClock(s.PTS - mediaOffset)
		if clk < 0 {
			continue // before the edit window: not presented
		}
		s.Clock = clk
		kept = append(kept, s)
	}
	t.Samples = kept
}
