// Package repair fixes broken media clocks and builds random-access indexes.
// Source timestamps are never overwritten: every sample keeps its source tick
// and gains a repaired tick on the unified timeline.
package repair

import (
	"archive.local/archive/internal/model"
	mp4 "archive.local/archive/internal/mp4"
)

// Sample is a repaired sample: source clock retained, playback clock fixed.
type Sample struct {
	Index       int
	SourceTicks int64 // as recorded in the source, unchanged
	Unified     int64 // repaired playback time on the unified clock
	Repaired    bool
	Size        uint32
	Offset      uint64
	Sync        bool
}

// ClockReport summarises what the clock repair changed.
type ClockReport struct {
	Samples    int
	Regressed  int // source timestamps that went backwards
	Missing    int // zero/absent deltas that needed a synthetic delta
	Gaps       int // discontinuities larger than the gap threshold
	Rewritten  int // total samples whose playback time differs from source
	FirstError *model.Error
}

// RepairClock rebuilds a monotonic playback clock from a track's sample table.
// A non-zero origin is preserved; regressions and zero deltas are repaired
// using the track's median delta while the source ticks stay intact. Deltas
// larger than maxGap media ticks are counted as discontinuities but left in
// place, so the index can expose the gap instead of hiding it.
func RepairClock(tr *mp4.Track, tl *model.Timeline, maxGap int64) ([]Sample, ClockReport, error) {
	rep := ClockReport{Samples: len(tr.Samples)}
	if len(tr.Samples) == 0 {
		return nil, rep, &model.Error{Code: model.CodeTruncatedSample, File: "", Track: tr.TrackID,
			Sample: 0, OffsetBytes: -1, Detail: "track has no samples"}
	}
	median := medianDelta(tr.Samples)
	if median <= 0 {
		median = 1
	}
	out := make([]Sample, len(tr.Samples))
	// Two clocks are tracked: the source ticks as recorded, used only to
	// detect regressions, and the repaired ticks used for playback.
	srcPrev := tr.Samples[0].SourceTicks
	repPrev := srcPrev
	playPrev, err := tl.Unified(tr.TrackID, repPrev)
	if err != nil {
		return nil, rep, clockErr(tr, err, 0)
	}
	out[0] = Sample{Index: 0, SourceTicks: srcPrev, Size: tr.Samples[0].Size,
		Offset: tr.Samples[0].Offset, Sync: tr.Samples[0].Sync, Unified: playPrev}
	for i := 1; i < len(tr.Samples); i++ {
		s := &tr.Samples[i]
		src := s.SourceTicks
		out[i] = Sample{Index: i, SourceTicks: src, Size: s.Size, Offset: s.Offset, Sync: s.Sync}
		delta := src - srcPrev
		var repDelta int64
		repaired := false
		switch {
		case delta < 0:
			rep.Regressed++
			repDelta, repaired = median, true
		case delta == 0:
			rep.Missing++
			repDelta, repaired = median, true
		case maxGap > 0 && delta > maxGap:
			rep.Gaps++
			repDelta = delta
		default:
			repDelta = delta
		}
		if repDelta > (1<<62)-repPrev {
			err := &model.Error{Code: model.CodeTimescaleOverflow, Track: tr.TrackID, Sample: i,
				OffsetBytes: -1, Detail: "repaired source clock overflows",
				Recoverable: &model.Recoverable{FirstSample: 0, LastSample: i}}
			rep.FirstError = err
			rep.Rewritten++
			return out[:i], rep, err
		}
		repPrev += repDelta
		u, err := tl.Unified(tr.TrackID, repPrev)
		if err != nil {
			me := clockErr(tr, err, i)
			rep.FirstError = me
			rep.Rewritten++
			return out[:i], rep, me
		}
		// Scaling may truncate, but playback must never run backwards.
		if u <= playPrev {
			u = playPrev + 1
		}
		out[i].Unified = u
		out[i].Repaired = repaired
		if repaired {
			rep.Rewritten++
		}
		srcPrev = src
		playPrev = u
	}
	return out, rep, nil
}

func clockErr(tr *mp4.Track, err error, i int) *model.Error {
	if me, ok := err.(*model.Error); ok {
		return &model.Error{Code: me.Code, Detail: me.Detail, File: me.File, Track: tr.TrackID,
			Sample: i, OffsetBytes: me.OffsetBytes, Err: me.Err,
			Recoverable: &model.Recoverable{FirstSample: 0, LastSample: i}}
	}
	return &model.Error{Code: model.CodeTimescaleOverflow, Track: tr.TrackID, Sample: i, OffsetBytes: -1,
		Detail: err.Error(), Recoverable: &model.Recoverable{FirstSample: 0, LastSample: i}}
}

func medianDelta(samples []mp4.Sample) int64 {
	if len(samples) < 2 {
		if len(samples) == 1 {
			return int64(samples[0].Duration)
		}
		return 0
	}
	vals := make([]int64, 0, len(samples)-1)
	for i := 1; i < len(samples); i++ {
		d := samples[i].SourceTicks - samples[i-1].SourceTicks
		if d > 0 {
			vals = append(vals, d)
		}
	}
	if len(vals) == 0 {
		return int64(samples[0].Duration)
	}
	for i := 1; i < len(vals); i++ {
		v := vals[i]
		j := i - 1
		for j >= 0 && vals[j] > v {
			vals[j+1] = vals[j]
			j--
		}
		vals[j+1] = v
	}
	return vals[len(vals)/2]
}
