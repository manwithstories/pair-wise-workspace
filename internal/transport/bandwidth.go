package transport

import "fmt"

// Representation is one bitrate tier of the adaptive stream.
type Representation struct {
	Name    string
	Bitrate int64 // bits per second
}

// Estimator is an O(1)-memory EWMA bandwidth estimator. It is fed received-byte
// deltas on the shared simulation clock.
type Estimator struct {
	alpha       float64
	tickRate    int64
	ewmaBps     float64
	initialized bool
	lastTicks   int64
	haveLast    bool
}

// NewEstimator builds an estimator. alpha is the EWMA weight for new samples;
// tickRate is the shared-clock tick frequency (ticks per second).
func NewEstimator(alpha float64, tickRate int64) *Estimator {
	if alpha <= 0 || alpha > 1 {
		alpha = 0.3
	}
	return &Estimator{alpha: alpha, tickRate: tickRate}
}

// Observe records that payloadBytes arrived between the previous call and nowTicks.
func (e *Estimator) Observe(nowTicks int64, payloadBytes int) {
	if !e.haveLast {
		e.lastTicks, e.haveLast = nowTicks, true
		return
	}
	dt := nowTicks - e.lastTicks
	e.lastTicks = nowTicks
	if dt <= 0 || payloadBytes <= 0 {
		return
	}
	bps := float64(payloadBytes) * 8 * float64(e.tickRate) / float64(dt)
	if !e.initialized {
		e.ewmaBps, e.initialized = bps, true
		return
	}
	e.ewmaBps = e.alpha*bps + (1-e.alpha)*e.ewmaBps
}

// BitsPerSecond returns the smoothed estimate (0 before any sample).
func (e *Estimator) BitsPerSecond() int64 {
	if !e.initialized {
		return 0
	}
	return int64(e.ewmaBps)
}

// Selector picks a representation from the bandwidth estimate with hysteresis:
// downswitches are immediate to protect continuity, upswitches need headroom
// and a hold period so a brief recovery cannot flap the tier.
type Selector struct {
	reps       []Representation // ascending bitrate
	headroom   float64          // fraction of spare capacity required to upswitch
	holdTicks  int64
	current    int
	lastSwitch int64
	switches   int
}

// NewSelector requires representations sorted by ascending bitrate.
func NewSelector(reps []Representation, holdTicks int64) (*Selector, error) {
	if len(reps) == 0 {
		return nil, fmt.Errorf("selector needs at least one representation")
	}
	for i := 1; i < len(reps); i++ {
		if reps[i].Bitrate <= reps[i-1].Bitrate {
			return nil, fmt.Errorf("representations must be ascending by bitrate")
		}
	}
	return &Selector{reps: reps, headroom: 0.1, holdTicks: holdTicks, current: 0, lastSwitch: -holdTicks}, nil
}

// Choose returns the selected representation index and whether it changed.
// downswitch fires immediately; upswitch respects headroom and hold time.
func (s *Selector) Choose(bps int64, nowTicks int64) (int, bool) {
	affordable := 0
	for i, r := range s.reps {
		if r.Bitrate <= bps {
			affordable = i
		}
	}
	want := s.current
	switch {
	case affordable < s.current:
		want = affordable // immediate downswitch
	case affordable > s.current &&
		nowTicks-s.lastSwitch >= s.holdTicks &&
		float64(s.reps[affordable].Bitrate) <= float64(bps)*(1-s.headroom):
		want = affordable
	}
	if want == s.current {
		return s.current, false
	}
	s.current, s.lastSwitch, s.switches = want, nowTicks, s.switches+1
	return want, true
}

// Current returns the active representation index.
func (s *Selector) Current() int { return s.current }

// SwitchCount returns how many tier changes happened.
func (s *Selector) SwitchCount() int { return s.switches }
