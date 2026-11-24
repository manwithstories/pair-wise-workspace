package scan

import (
	"mediaroute/internal/clock"
	"mediaroute/internal/index"
	"mediaroute/internal/manifest"
)

// rangeBuilder coalesces consecutive unreadable samples into one corrupt range
// per track, preserving the first offending byte offset.
type rangeBuilder struct {
	tb         clock.Timebase
	trackID    uint32
	editOffset int64
	current    *index.CorruptRange
	out        []index.CorruptRange
}

func newRangeBuilder(tb clock.Timebase, trackID uint32, editOffset int64) *rangeBuilder {
	return &rangeBuilder{tb: tb, trackID: trackID, editOffset: editOffset}
}

func (r *rangeBuilder) add(sample, offset int64, reason string, start, end clock.Rational) {
	if r.current == nil {
		r.current = &index.CorruptRange{
			TrackID:     r.trackID,
			FirstSample: sample,
			LastSample:  sample,
			ByteOffset:  offset,
			Reason:      reason,
			Start:       start,
			End:         end,
		}
		return
	}
	if r.current.Reason == reason && sample == r.current.LastSample+1 {
		r.current.LastSample = sample
		r.current.End = end
		return
	}
	r.flush()
	r.current = &index.CorruptRange{
		TrackID:     r.trackID,
		FirstSample: sample,
		LastSample:  sample,
		ByteOffset:  offset,
		Reason:      reason,
		Start:       start,
		End:         end,
	}
}

func (r *rangeBuilder) flush() {
	if r.current != nil {
		r.out = append(r.out, *r.current)
		r.current = nil
	}
}

func (r *rangeBuilder) close() []index.CorruptRange {
	r.flush()
	return r.out
}

// Layout returns the declared audio layout, defaulting to "unknown".
func layoutOf(mt manifest.Track) string {
	if mt.Audio == nil || mt.Audio.Layout == "" {
		return "unknown"
	}
	return mt.Audio.Layout
}
