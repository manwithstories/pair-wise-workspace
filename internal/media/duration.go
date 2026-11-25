package media

// AssignDurations fills zero sample durations from monotone decode-time gaps.
// It preserves variable frame intervals rather than forcing a nominal FPS.
func AssignDurations(samples []Sample, fallback uint32) {
	if fallback == 0 {
		fallback = 3000
	}
	lastIndex := map[uint16]int{}
	haveLast := map[uint16]bool{}
	lastGap := map[uint16]int64{}
	for i := range samples {
		pid := samples[i].PID
		if haveLast[pid] {
			prev := lastIndex[pid]
			if gap := samples[i].DTS - samples[prev].DTS; gap > 0 && gap < 900000 {
				d := uint32(gap)
				samples[prev].Duration = d
				lastGap[pid] = gap
			}
		}
		lastIndex[pid] = i
		haveLast[pid] = true
	}
	for pid, idx := range lastIndex {
		if samples[idx].Duration == 0 {
			if gap := lastGap[pid]; gap > 0 {
				samples[idx].Duration = uint32(gap)
			} else {
				samples[idx].Duration = fallback
			}
		}
	}
}

// DurationMS returns the inclusive timeline duration in rounded milliseconds.
func DurationMS(samples []Sample) int64 {
	if len(samples) == 0 {
		return 0
	}
	var minDTS, maxEnd int64
	init := false
	for _, s := range samples {
		end := s.DTS + int64(s.Duration)
		if !init {
			minDTS, maxEnd, init = s.DTS, end, true
		}
		if s.DTS < minDTS {
			minDTS = s.DTS
		}
		if end > maxEnd {
			maxEnd = end
		}
	}
	return Ms(maxEnd-minDTS, Rational{Num: 1, Den: 90000})
}
