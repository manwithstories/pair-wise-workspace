package timeline

import (
	"fmt"
	"sort"

	"relayaudit/internal/clock"
	"relayaudit/internal/fault"
	"relayaudit/internal/wire"
)

// BuildTracks places records on per-track wrap-extended timelines. The
// returned origins make every track's first record start at zero, so values
// are directly comparable across tracks.
func BuildTracks(records []*wire.Record) (map[uint8]*Track, error) {
	tracks := map[uint8]*Track{}
	perTrackRaw := map[uint8][]*wire.Record{}
	for _, rec := range records {
		perTrackRaw[rec.Track] = append(perTrackRaw[rec.Track], rec)
	}
	// Extend every track from the first record's raw stamp. Using the raw
	// minimum instead would be wrong whenever the segment spans the 32-bit
	// wrap, because wrapped stamps are numerically tiny.
	if len(records) == 0 {
		return tracks, nil
	}
	base := uint64(records[0].PTS)
	var minAbs uint64 = base
	for id, recs := range perTrackRaw {
		tr := &Track{ID: id}
		prev := base
		for _, rec := range recs {
			abs, _ := clock.Extend(prev, rec.PTS)
			if len(tr.Entries) > 0 && abs < prev {
				return nil, fault.New(fault.Length, rec.Offset,
					"track %d pts regressed from %d to %d", id, prev, abs).WithTrack(id)
			}
			prev = abs
			tr.Entries = append(tr.Entries, Entry{Rec: rec, Start: abs})
		}
		if tr.Entries[0].Start < minAbs {
			minAbs = tr.Entries[0].Start
		}
		tracks[id] = tr
	}
	// Normalise every track onto the earliest extended tick so clip ticks
	// taken from one track mean the same instant on another.
	for id, tr := range tracks {
		for i := range tr.Entries {
			if tr.Entries[i].Start >= minAbs {
				tr.Entries[i].Start -= minAbs
			} else {
				tr.Entries[i].Start = 0
			}
		}
		for i := range tr.Entries {
			if i+1 < len(tr.Entries) {
				tr.Entries[i].End = tr.Entries[i+1].Start
			} else {
				tr.Entries[i].End = tr.Entries[i].Start + defaultFrameTicks(id)
			}
		}
		tr.First = 0
		tr.Last = tr.Entries[len(tr.Entries)-1].End
	}
	return tracks, nil
}

// Resolve binds plan to records. Misaligned boundaries are not fatal: they are
// returned as fault.Boundary faults carrying the original record offset, and
// the affected clip is widened to whole records.
func Resolve(records []*wire.Record, plan Plan) (*Resolution, []*fault.Fault, error) {
	tracks, err := BuildTracks(records)
	if err != nil {
		return nil, nil, err
	}
	res := &Resolution{Tracks: tracks}
	for _, tr := range tracks {
		if tr.Last > res.Duration {
			res.Duration = tr.Last
		}
	}
	var faults []*fault.Fault
	for _, c := range plan.Clips {
		tr, ok := tracks[c.Track]
		if !ok {
			return nil, nil, fmt.Errorf("clip %q references unknown track %d", c.Label, c.Track)
		}
		adj, fs := adjustBounds(tr, c.Start, c.End)
		faults = append(faults, fs...)
		c.StartAdjusted = adj.startAdjusted
		c.EndAdjusted = adj.endAdjusted
		c.Start, c.End = adj.start, adj.end
		res.Clips = append(res.Clips, c)
	}
	for _, t := range plan.Transitions {
		from, ok := tracks[t.From]
		if !ok {
			return nil, nil, fmt.Errorf("transition %q references unknown track %d", t.Label, t.From)
		}
		to, ok := tracks[t.To]
		if !ok {
			return nil, nil, fmt.Errorf("transition %q references unknown track %d", t.Label, t.To)
		}
		adj, fs := adjustBounds(from, t.Start, t.End)
		faults = append(faults, fs...)
		t.StartAdjusted = adj.startAdjusted
		t.EndAdjusted = adj.endAdjusted
		t.Start, t.End = adj.start, adj.end
		// The partner track must be able to cover the transition window.
		if _, fs2 := adjustBounds(to, t.Start, t.End); len(fs2) > 0 {
			faults = append(faults, fs2...)
		}
		res.Transitions = append(res.Transitions, t)
	}
	sortFaults(faults)
	return res, faults, nil
}

type bounds struct {
	start, end       uint64
	startAdjusted    bool
	endAdjusted      bool
	startRec, endRec *wire.Record
}

// adjustBounds widens [start,end] so both edges coincide with record edges.
func adjustBounds(tr *Track, start, end uint64) (bounds, []*fault.Fault) {
	b := bounds{start: start, end: end}
	var fs []*fault.Fault
	for i := range tr.Entries {
		e := &tr.Entries[i]
		if start > e.Start && start < e.End {
			b.start = e.Start
			b.startAdjusted = true
			b.startRec = e.Rec
			fs = append(fs, fault.New(fault.Boundary, e.Rec.Offset,
				"clip start %d falls inside record [%d,%d) on track %d",
				start, e.Start, e.End, tr.ID).WithTrack(tr.ID).WithLength(e.Rec.Size()))
			break
		}
	}
	for i := range tr.Entries {
		e := &tr.Entries[i]
		if end > e.Start && end < e.End {
			b.end = e.End
			b.endAdjusted = true
			b.endRec = e.Rec
			fs = append(fs, fault.New(fault.Boundary, e.Rec.Offset,
				"clip end %d falls inside record [%d,%d) on track %d",
				end, e.Start, e.End, tr.ID).WithTrack(tr.ID).WithLength(e.Rec.Size()))
			break
		}
	}
	return b, fs
}

// FaultRecordOffsets returns the byte offsets referenced by boundary faults.
func FaultRecordOffsets(faults []*fault.Fault) []int64 {
	out := make([]int64, 0, len(faults))
	for _, f := range faults {
		out = append(out, f.Offset())
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sortFaults(fs []*fault.Fault) {
	sort.Slice(fs, func(i, j int) bool { return fs[i].Offset() < fs[j].Offset() })
}
