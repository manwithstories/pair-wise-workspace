package mp4atom

// applyEdits maps media-axis PTS onto a continuous presentation axis.
//
// Empty edits (media_time == -1) insert a gap at the start of the timeline and
// are counted separately. A normal edit declares a media_time offset into the
// track and a segment duration in the movie timescale, which is converted
// independently of the media timescale. Negative ctts can place a sample's PTS
// before the edit's media_time (B-frame reorder); such samples are clamped onto
// the continuous axis rather than dropped. The media axis (PTS) and the
// presentation axis (PresentMs) are both retained.
func applyEdits(t *Table, samples []Sample, tl *Timeline) error {
	n := len(samples)
	tl.PresentMs = make([]int64, n)
	if len(t.Elst) == 0 {
		for i, s := range samples {
			tl.PresentMs[i] = scaleMs(s.Pts, t.Timescale)
		}
		tl.DurationMs = lastPlusDelta(t, samples)
		return nil
	}
	var timelineBase int64
	cursor := 0
	for _, e := range t.Elst {
		if e.MediaTime < 0 {
			tl.EmptyEdits++
			timelineBase += convertUnits(int64(e.SegmentDuration),
				t.MovieTS, t.Timescale)
			continue
		}
		start := e.MediaTime
		seg := convertUnits(int64(e.SegmentDuration), t.MovieTS, t.Timescale)
		end := start + seg
		for cursor < n {
			pts := samples[cursor].Pts
			if pts >= end {
				break
			}
			rel := pts - start
			if rel < 0 {
				rel = 0
			}
			tl.PresentMs[cursor] =
				scaleMs(timelineBase+rel, t.Timescale)
			cursor++
		}
		timelineBase += seg
	}
	if cursor < n {
		base := tl.PresentMs[cursor-1]
		refPts := samples[cursor-1].Pts
		for i := cursor; i < n; i++ {
			tl.PresentMs[i] = base +
				scaleMs(samples[i].Pts-refPts, t.Timescale)
		}
	}
	if n > 0 {
		tl.DurationMs = tl.PresentMs[n-1] +
			scaleMs(int64(lastDelta(t)), t.Timescale)
	} else {
		tl.DurationMs = scaleMs(timelineBase, t.Timescale)
	}
	return nil
}

// convertUnits rescales a duration from one timescale to another.
func convertUnits(v int64, fromTS, toTS uint32) int64 {
	if fromTS == 0 || toTS == 0 {
		return v
	}
	return v * int64(toTS) / int64(fromTS)
}

func lastPlusDelta(t *Table, samples []Sample) int64 {
	if len(samples) == 0 {
		return 0
	}
	last := samples[len(samples)-1]
	return scaleMs(last.Pts+int64(lastDelta(t)), t.Timescale)
}

func lastDelta(t *Table) uint32 {
	for i := len(t.Stts) - 1; i >= 0; i-- {
		if t.Stts[i].Count > 0 {
			return t.Stts[i].Delta
		}
	}
	return 0
}
