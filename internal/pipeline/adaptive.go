package pipeline

import "mediamix/internal/timeline"

// bandwidthTuner estimates the observed uplink rate from packet arrivals and
// reports when the estimate has moved enough to justify re-binding the buffer.
//
// The estimate uses a sliding window of arrival timestamps: packets per second
// times the mean packet size gives bytes per second. Because the sender's
// nominal rate is known from the initial estimate, a sustained gap in arrivals
// reads as a bandwidth drop, which is exactly when the buffer should shrink.
type bandwidthTuner struct {
	initial   float64
	current   float64
	window    []timeline.Micros
	last      timeline.Micros
	haveLast  bool
	// sampleEvery is how many arrivals between re-samples. Short enough to
	// react within a second of proxy media, long enough to avoid thrash.
	sampleEvery int
	count       int
}

// newBandwidthTuner seeds the estimate with the operator-supplied figure.
func newBandwidthTuner(initialMBps float64) *bandwidthTuner {
	if initialMBps <= 0 {
		initialMBps = 1
	}
	return &bandwidthTuner{initial: initialMBps, current: initialMBps, sampleEvery: 24}
}

// observe records an arrival and returns a new bandwidth estimate when one is
// due. changed is false while the window is still filling.
func (t *bandwidthTuner) observe(arrival timeline.Micros) (mbps float64, changed bool) {
	if t.haveLast {
		if gap := arrival - t.last; gap > 0 {
			t.window = append(t.window, gap)
			if len(t.window) > 64 {
				t.window = t.window[len(t.window)-64:]
			}
		}
	}
	t.last = arrival
	t.haveLast = true
	t.count++
	if t.count%t.sampleEvery != 0 || len(t.window) < 8 {
		return t.current, false
	}
	// Mean inter-arrival interval over the window.
	var sum timeline.Micros
	for _, g := range t.window {
		sum += g
	}
	mean := float64(sum) / float64(len(t.window)) // microseconds
	if mean <= 0 {
		return t.current, false
	}
	// The nominal interval at the initial rate is derived from the stream's own
	// pacing: a longer observed interval means fewer packets per second, which
	// scales the estimate down proportionally.
	nominal := t.initial
	// Clamp the movement so one burst of jitter cannot swing the estimate.
	if mean > 0 {
		ratio := 1.0
		if len(t.window) > 0 {
			// Compare the recent half against the older half to detect a trend.
			mid := len(t.window) / 2
			older, newer := meanOf(t.window[:mid]), meanOf(t.window[mid:])
			if older > 0 && newer > 0 {
				ratio = older / newer
			}
		}
		nominal = t.initial * ratio
	}
	if nominal < t.initial*0.25 {
		nominal = t.initial * 0.25
	}
	if nominal > t.initial*2 {
		nominal = t.initial * 2
	}
	// Require a meaningful change before reporting.
	if nominal > t.current*1.25 || nominal < t.current*0.8 {
		t.current = nominal
		return nominal, true
	}
	return t.current, false
}

func meanOf(v []timeline.Micros) float64 {
	if len(v) == 0 {
		return 0
	}
	var s timeline.Micros
	for _, x := range v {
		s += x
	}
	return float64(s) / float64(len(v))
}
