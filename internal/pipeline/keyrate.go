package pipeline

// KeyframeHitRate returns the percentage of segments that begin with an IDR
// listed in the index. A stream with no segments scores 0.
func (ix *Index) KeyframeHitRate(segs []Segment) float64 {
	if len(segs) == 0 {
		return 0
	}
	hit := 0
	for _, s := range segs {
		if s.Keyframe {
			for _, kf := range ix.Keyframes {
				if kf.Segment == s.Index && kf.PTS == s.StartPTS {
					hit++
					break
				}
			}
		}
	}
	return float64(hit) * 100 / float64(len(segs))
}
