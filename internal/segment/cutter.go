// Package segment splits normalized media records into fixed clock windows.
// Cut points are derived solely from the shared media clock; audio silence
// information never moves a video boundary.
package segment

import (
	"medpipe/internal/model"
)

// Cutter partitions records into windows of WindowNS nanoseconds.
type Cutter struct {
	WindowNS int64
}

// New validates the window and returns a ready cutter.
func New(windowNS int64) (*Cutter, error) {
	if windowNS <= 0 {
		return nil, model.NewError(model.ErrKind, "window must be positive, got %d", windowNS)
	}
	return &Cutter{WindowNS: windowNS}, nil
}

// Cut assigns every video frame and audio block to the clock window its
// normalized timestamp falls in. Empty windows are skipped, so segment
// indices stay contiguous over real data. Determinism is guaranteed because
// inputs are already in timeline order.
func (c *Cutter) Cut(frames []*model.Frame, audio []*model.AudioBlock) []model.Segment {
	buckets := map[int64]*model.Segment{}
	var order []int64

	assign := func(ts int64, add func(seg *model.Segment)) {
		key := ts / c.WindowNS
		seg, ok := buckets[key]
		if !ok {
			seg = &model.Segment{
				Index:   len(order),
				StartNS: key * c.WindowNS,
				EndNS:   (key + 1) * c.WindowNS,
			}
			buckets[key] = seg
			order = append(order, key)
		}
		add(seg)
	}

	for _, f := range frames {
		assign(f.TimestampNS, func(seg *model.Segment) {
			seg.VideoSeqs = append(seg.VideoSeqs, f.Seq)
		})
	}
	for _, a := range audio {
		assign(a.TimestampNS, func(seg *model.Segment) {
			seg.AudioSeqs = append(seg.AudioSeqs, a.Seq)
		})
	}

	out := make([]model.Segment, 0, len(order))
	for i, key := range order {
		seg := *buckets[key]
		seg.Index = i
		out = append(out, seg)
	}
	return out
}

// VideoCutPoints returns the window start timestamps that bound video-only
// cut decisions. Silence intervals may be reported alongside but are passed
// in elsewhere; this function intentionally ignores them.
func (c *Cutter) VideoCutPoints(frames []*model.Frame) []int64 {
	if len(frames) == 0 {
		return nil
	}
	var points []int64
	last := int64(-1)
	for _, f := range frames {
		key := f.TimestampNS / c.WindowNS
		if key != last {
			points = append(points, key*c.WindowNS)
			last = key
		}
	}
	return points
}
