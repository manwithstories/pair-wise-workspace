package media

// Interval is a time range in milliseconds on the unified clock.
type Interval struct {
	StartMs int64
	EndMs   int64
}

// DetectSilence scans interleaved multi-channel PCM (channels x samples)
// and returns intervals where every channel stays below the peak threshold
// for at least minDurMs. Boundaries always fall on sample points.
//
// samples[ch] holds one channel's samples; channels must have equal length.
// threshold is a peak amplitude in [0,32767]; minDurMs is the gate.
func DetectSilence(samples [][]int16, sampleRate int, threshold int16, minDurMs int64) []Interval {
	if len(samples) == 0 || sampleRate <= 0 {
		return nil
	}
	n := len(samples[0])
	for _, ch := range samples {
		if len(ch) < n {
			n = len(ch)
		}
	}
	if n == 0 {
		return nil
	}
	// 10ms analysis windows, aligned to sample points.
	win := sampleRate / 100
	if win < 1 {
		win = 1
	}
	minWin := int(minDurMs * int64(sampleRate) / 1000 / int64(win))
	if minWin < 1 {
		minWin = 1
	}
	var out []Interval
	runStart := -1
	runLen := 0
	flush := func(endWin int) {
		if runLen >= minWin {
			out = append(out, Interval{
				StartMs: int64(runStart) * int64(win) * 1000 / int64(sampleRate),
				EndMs:   int64(endWin) * int64(win) * 1000 / int64(sampleRate),
			})
		}
		runStart, runLen = -1, 0
	}
	for w := 0; w*win < n; w++ {
		silent := true
		for _, ch := range samples {
			lo, hi := w*win, (w+1)*win
			if hi > len(ch) {
				hi = len(ch)
			}
			for _, s := range ch[lo:hi] {
				a := s
				if a < 0 {
					a = -a
				}
				if a > threshold {
					silent = false
					break
				}
			}
			if !silent {
				break
			}
		}
		if silent {
			if runStart < 0 {
				runStart = w
			}
			runLen++
		} else if runStart >= 0 {
			flush(w)
		}
	}
	if runStart >= 0 {
		flush((n + win - 1) / win)
	}
	return out
}
