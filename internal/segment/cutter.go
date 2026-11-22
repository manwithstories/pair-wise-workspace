// Package segment cuts reusable ranges out of an explicit timeline.
// A boundary that lands inside a segment is snapped backwards to the
// nearest keyframe sample boundary; the deviation is reported.
package segment

import (
	"mediacore/internal/clock"
	"mediacore/internal/media"
	"mediacore/internal/timeline"
)

// Request describes the desired cut range on one track.
type Request struct {
	TrackID string
	Start   clock.Tick
	End     clock.Tick
}

// Boundary is a resolved cut point.
type Boundary struct {
	Requested    clock.Tick
	Actual       clock.Tick
	SegmentIndex int
	SampleIndex  int
}

// DeviationMs returns how far the actual boundary moved backwards.
func (b Boundary) DeviationMs() float64 {
	d, err := b.Requested.Sub(b.Actual)
	if err != nil {
		return 0
	}
	return d.Millis()
}

// Result is the outcome of a cut.
type Result struct {
	TrackID  string
	Start    Boundary
	End      Boundary
	Segments []timeline.TSegment // timeline segments inside the actual range
}

// Cut resolves req against the track timeline and the per-segment
// sample indexes (idx[i] belongs to tt.Segments[i], nil for gaps).
func Cut(tt *timeline.TrackTimeline, idx []*media.Segment, req Request) (*Result, error) {
	const op = "cut"
	if req.Start.Sign() < 0 {
		return nil, clock.At(clock.CodeManifestInvalid, op, req.TrackID, -1,
			"negative cut start %s", req.Start)
	}
	if req.End.Cmp(req.Start) <= 0 {
		return nil, clock.At(clock.CodeManifestInvalid, op, req.TrackID, -1,
			"empty cut range [%s,%s)", req.Start, req.End)
	}
	if len(idx) != len(tt.Segments) {
		return nil, clock.At(clock.CodeDependencyMissing, op, req.TrackID, -1,
			"sample index count %d != segment count %d", len(idx), len(tt.Segments))
	}
	start, err := locate(tt, idx, req.Start)
	if err != nil {
		return nil, err
	}
	end, err := locate(tt, idx, req.End)
	if err != nil {
		return nil, err
	}
	res := &Result{TrackID: req.TrackID, Start: start, End: end}
	for _, seg := range tt.Segments {
		if seg.Start.Cmp(start.Actual) >= 0 && seg.End.Cmp(end.Actual) <= 0 {
			res.Segments = append(res.Segments, seg)
		}
	}
	return res, nil
}

// locate snaps t backwards to the nearest keyframe sample boundary.
func locate(tt *timeline.TrackTimeline, idx []*media.Segment, t clock.Tick) (Boundary, error) {
	const op = "cut"
	b := Boundary{Requested: t, SegmentIndex: -1, SampleIndex: -1}
	if len(tt.Segments) == 0 {
		return b, clock.At(clock.CodeManifestInvalid, op, tt.Meta.ID, -1, "empty track")
	}
	last := tt.Segments[len(tt.Segments)-1]
	if t.Cmp(last.End) >= 0 {
		b.Actual = last.End
		b.SegmentIndex = last.Index
		return b, nil
	}
	for i, seg := range tt.Segments {
		if t.Cmp(seg.Start) < 0 || t.Cmp(seg.End) >= 0 {
			continue
		}
		if seg.Gap || idx[i] == nil {
			// A boundary inside a hole snaps to the hole start.
			b.Actual = seg.Start
			b.SegmentIndex = seg.Index
			return b, nil
		}
		acc := seg.Start
		found := false
		for j, smp := range idx[i].Samples {
			if acc.Cmp(t) > 0 {
				break
			}
			if smp.Keyframe {
				b.Actual = acc
				b.SegmentIndex = seg.Index
				b.SampleIndex = j
				found = true
			}
			d, err := clock.New(int64(smp.Duration), tt.Meta.Timescale)
			if err != nil {
				return b, clock.Wrap(clock.CodeTickOverflow, op, err)
			}
			if acc, err = acc.Add(d); err != nil {
				return b, clock.Wrap(clock.CodeTickOverflow, op, err)
			}
		}
		if found {
			return b, nil
		}
		// No keyframe inside this segment: walk backwards.
		for k := i - 1; k >= 0; k-- {
			if tt.Segments[k].Gap || idx[k] == nil {
				continue
			}
			acc := tt.Segments[k].Start
			for j, smp := range idx[k].Samples {
				if smp.Keyframe {
					b.Actual = acc
					b.SegmentIndex = tt.Segments[k].Index
					b.SampleIndex = j
					found = true
				}
				d, err := clock.New(int64(smp.Duration), tt.Meta.Timescale)
				if err != nil {
					return b, clock.Wrap(clock.CodeTickOverflow, op, err)
				}
				if acc, err = acc.Add(d); err != nil {
					return b, clock.Wrap(clock.CodeTickOverflow, op, err)
				}
			}
			if found {
				return b, nil
			}
		}
		return b, clock.At(clock.CodeInputQuality, op, tt.Meta.ID, seg.Index,
			"no keyframe at or before %s", t)
	}
	// t before the first segment start.
	b.Actual = tt.Segments[0].Start
	b.SegmentIndex = tt.Segments[0].Index
	return b, nil
}
