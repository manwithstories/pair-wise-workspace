package timeline

import (
	"sort"

	"relayaudit/internal/wire"
)

// EntryAt returns the entry containing tick on track.
func (r *Resolution) EntryAt(track uint8, tick uint64) (Entry, bool) {
	tr, ok := r.Tracks[track]
	if !ok {
		return Entry{}, false
	}
	i := sort.Search(len(tr.Entries), func(i int) bool { return tr.Entries[i].End > tick })
	if i >= len(tr.Entries) || tick < tr.Entries[i].Start {
		return Entry{}, false
	}
	return tr.Entries[i], true
}

// LabelsFor lists the clips and transitions covering a record on its own
// track. Labels are stable and sorted.
func (r *Resolution) LabelsFor(rec *wire.Record) []string {
	tick := r.tickForRecord(rec)
	var out []string
	for _, c := range r.Clips {
		if c.Track == rec.Track && tick >= c.Start && tick < c.End {
			out = append(out, c.Label)
		}
	}
	for _, t := range r.Transitions {
		if (t.From == rec.Track || t.To == rec.Track) && tick >= t.Start && tick < t.End {
			out = append(out, t.Label)
		}
	}
	if out == nil {
		out = []string{}
	}
	sort.Strings(out)
	return out
}

// tickForRecord returns the shared tick of rec.
func (r *Resolution) tickForRecord(rec *wire.Record) uint64 {
	tr, ok := r.Tracks[rec.Track]
	if !ok {
		return 0
	}
	i := sort.Search(len(tr.Entries), func(i int) bool { return tr.Entries[i].Rec.Offset >= rec.Offset })
	if i < len(tr.Entries) && tr.Entries[i].Rec.Offset == rec.Offset {
		return tr.Entries[i].Start
	}
	return 0
}

// TickForOffset returns the shared tick where the record starting at offset
// begins on track.
func (r *Resolution) TickForOffset(track uint8, offset int64) (uint64, bool) {
	tr, ok := r.Tracks[track]
	if !ok {
		return 0, false
	}
	i := sort.Search(len(tr.Entries), func(i int) bool { return tr.Entries[i].Rec.Offset >= offset })
	if i >= len(tr.Entries) || tr.Entries[i].Rec.Offset != offset {
		return 0, false
	}
	return tr.Entries[i].Start, true
}

// TickNearOffset maps a source offset that may fall inside or between
// records to the tick of the nearest preceding record. Damaged records that
// no longer parse still map to the editorial instant they occupied.
func (r *Resolution) TickNearOffset(track uint8, offset int64) (uint64, bool) {
	if tick, ok := r.TickForOffset(track, offset); ok {
		return tick, true
	}
	tr, ok := r.Tracks[track]
	if !ok || len(tr.Entries) == 0 {
		return 0, false
	}
	i := sort.Search(len(tr.Entries), func(i int) bool { return tr.Entries[i].Rec.Offset > offset })
	if i == 0 {
		return tr.Entries[0].Start, true
	}
	e := tr.Entries[i-1]
	delta := offset - e.Rec.Offset
	if delta > e.Rec.Size() {
		// Offset lies in a gap after this record; keep the record's own
		// start so labels stay on the clip that contained it.
		delta = 0
	}
	return e.Start + uint64(delta)*uint64(e.End-e.Start)/uint64(e.Rec.Size()), true
}

// SpansOverlapping returns clips and transitions that overlap the tick range.
func (r *Resolution) SpansOverlapping(track uint8, start, end uint64) []string {
	var out []string
	for _, c := range r.Clips {
		if c.Track == track && c.Start < end && start < c.End {
			out = append(out, c.Label)
		}
	}
	for _, t := range r.Transitions {
		if (t.From == track || t.To == track) && t.Start < end && start < t.End {
			out = append(out, t.Label)
		}
	}
	sort.Strings(out)
	return out
}

// SamplePlan derives an aligned four-track editorial plan from the records.
// Cuts are taken from real record boundaries, so the reference plan resolves
// without boundary faults; the cross transitions span the video and audio
// tracks over the same shared window.
func SamplePlan(records []*wire.Record) Plan {
	tracks, err := BuildTracks(records)
	if err != nil {
		return Plan{}
	}
	videoStarts := startsOf(tracks[1])
	audioStarts := startsOf(tracks[3])
	if len(videoStarts) < 8 || len(audioStarts) < 8 {
		return Plan{}
	}
	vi1 := len(videoStarts) / 3
	vi2 := len(videoStarts) * 2 / 3
	ai1 := len(audioStarts) / 3
	ai2 := len(audioStarts) * 2 / 3
	a := videoStarts[vi1]
	b := videoStarts[vi2]
	e := videoStarts[min(vi1+50, len(videoStarts)-1)]
	c := audioStarts[ai1]
	d := audioStarts[min(ai1+25, len(audioStarts)-1)]

	plan := Plan{
		Clips: []Clip{
			{Track: 1, Label: "prog-a", Start: 0, End: a},
			{Track: 1, Label: "prog-b", Start: b, End: tracks[1].Last},
			{Track: 2, Label: "backup-a", Start: e, End: b},
			{Track: 3, Label: "pa-a", Start: 0, End: c},
			{Track: 3, Label: "pa-b", Start: audioStarts[ai2], End: tracks[3].Last},
			{Track: 4, Label: "ba-a", Start: d, End: audioStarts[ai2]},
		},
		Transitions: []Transition{
			{From: 1, To: 2, Kind: "cross", Label: "x-video-1to2", Start: a, End: e},
			{From: 3, To: 4, Kind: "cross", Label: "x-audio-3to4", Start: c, End: d},
		},
	}
	return plan
}

// startsOf returns the shared start tick of every record on a track.
func startsOf(tr *Track) []uint64 {
	if tr == nil {
		return nil
	}
	out := make([]uint64, len(tr.Entries))
	for i, e := range tr.Entries {
		out[i] = e.Start
	}
	return out
}
