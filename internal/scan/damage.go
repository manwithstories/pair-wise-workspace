package scan

import (
	"avscan/internal/model"
)

// DamageResult holds located segments and the smallest failing byte offset.
type DamageResult struct {
	Segments      []model.DamageSegment
	FirstBadOffset int64 // -1 when no byte-located fault exists
}

// segmentGap bounds how far two faults on one track may be apart and still
// belong to one segment. It is expressed in whole samples so the locating
// error never exceeds one sample boundary.
const segmentGapSamples int64 = 2

// LocateDamage groups per-track faults into maximal contiguous segments.
// Every segment start/end snaps to a sample boundary; faults that only have a
// box-level byte offset are reported as single-point segments.
func LocateDamage(tl *model.Timeline, globalFaults []model.Fault) DamageResult {
	res := DamageResult{FirstBadOffset: -1}
	for _, f := range globalFaults {
		res.noteOffset(f)
		if f.HasByte {
			res.Segments = append(res.Segments, pointSegment(f))
		}
	}
	for _, tr := range tl.Tracks {
		// Parse-time faults plus any fault attached by later stages.
		all := append([]model.Fault(nil), tr.ParseFaults...)
		if len(all) == 0 {
			continue
		}
		res.Segments = append(res.Segments, segmentsForTrack(tr, all, &res)...)
	}
	return res
}

// segmentsForTrack sorts faults by sample index and merges near neighbours.
func segmentsForTrack(tr *model.Track, faults []model.Fault, res *DamageResult) []model.DamageSegment {
	indexed := make([]model.Fault, 0, len(faults))
	boxLevel := make([]model.Fault, 0)
	for _, f := range faults {
		res.noteOffset(f)
		if f.SampleIndex >= 0 {
			indexed = append(indexed, f)
		} else {
			boxLevel = append(boxLevel, f)
		}
	}
	// Faults are typically already ordered; insertion sort keeps it simple and
	// bounded by the fixed sample limit.
	for i := 1; i < len(indexed); i++ {
		for j := i; j > 0 && indexed[j-1].SampleIndex > indexed[j].SampleIndex; j-- {
			indexed[j-1], indexed[j] = indexed[j], indexed[j-1]
		}
	}
	var segs []model.DamageSegment
	var cur *model.DamageSegment
	var lastIdx int64 = -1
	for _, f := range indexed {
		if cur == nil || f.SampleIndex-lastIdx > segmentGapSamples {
			if cur != nil {
				segs = append(segs, *cur)
			}
			cur = newSegment(tr, f)
		} else {
			extendSegment(cur, tr, f)
		}
		addKind(cur, string(f.Kind))
		lastIdx = f.SampleIndex
	}
	if cur != nil {
		segs = append(segs, *cur)
	}
	for _, f := range boxLevel {
		segs = append(segs, pointSegment(f))
	}
	return segs
}

func newSegment(tr *model.Track, f model.Fault) *model.DamageSegment {
	seg := model.DamageSegment{
		Source: f.Source, TrackID: tr.ID, FaultCount: 1,
		Kinds: []string{string(f.Kind)},
	}
	if ev := sampleAt(tr, int(f.SampleIndex)); ev != nil {
		seg.StartOffset = int64(ev.Offset)
		seg.EndOffset = int64(ev.Offset) + int64(ev.Size)
		seg.StartNS = ev.StartNS
		seg.EndNS = ev.StartNS + ev.DurationNS
	}
	return &seg
}

func extendSegment(seg *model.DamageSegment, tr *model.Track, f model.Fault) {
	seg.FaultCount++
	if ev := sampleAt(tr, int(f.SampleIndex)); ev != nil {
		end := int64(ev.Offset) + int64(ev.Size)
		if seg.StartOffset == 0 || int64(ev.Offset) < seg.StartOffset {
			seg.StartOffset = int64(ev.Offset)
			seg.StartNS = ev.StartNS
		}
		if end > seg.EndOffset {
			seg.EndOffset = end
			seg.EndNS = ev.StartNS + ev.DurationNS
		}
	}
}

func sampleAt(tr *model.Track, idx int) *model.SampleEvent {
	if idx < 0 || idx >= len(tr.Samples) {
		return nil
	}
	return &tr.Samples[idx]
}

func pointSegment(f model.Fault) model.DamageSegment {
	seg := model.DamageSegment{
		Source: f.Source, TrackID: f.TrackID, FaultCount: 1,
		Kinds: []string{string(f.Kind)},
	}
	if f.HasByte {
		seg.StartOffset = f.ByteOffset
		seg.EndOffset = f.ByteOffset
	}
	if f.HasTime {
		seg.StartNS = f.TimeNS
		seg.EndNS = f.TimeNS
	}
	return seg
}

func (r *DamageResult) noteOffset(f model.Fault) {
	if f.HasByte && (r.FirstBadOffset == -1 || f.ByteOffset < r.FirstBadOffset) {
		r.FirstBadOffset = f.ByteOffset
	}
}

func addKind(s *model.DamageSegment, kind string) {
	for _, k := range s.Kinds {
		if k == kind {
			return
		}
	}
	s.Kinds = append(s.Kinds, kind)
}
