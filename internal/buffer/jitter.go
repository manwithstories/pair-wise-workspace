// Package buffer adapts jitter-buffer bounds to a sliding bandwidth
// estimate, protecting audio clock continuity on sudden drops.
package buffer

// Estimator is a sliding-window bandwidth estimator. All timestamps are
// caller-supplied milliseconds so behaviour stays deterministic.
type Estimator struct {
	WindowMs int64
	samples  []sample
	total    int64
}

type sample struct{ atMs, bytes int64 }

func NewEstimator(windowMs int64) *Estimator { return &Estimator{WindowMs: windowMs} }

// Add records bytes received at atMs.
func (e *Estimator) Add(atMs, bytes int64) {
	e.samples = append(e.samples, sample{atMs, bytes})
	e.total += bytes
	cut := atMs - e.WindowMs
	for len(e.samples) > 0 && e.samples[0].atMs < cut {
		e.total -= e.samples[0].bytes
		e.samples = e.samples[1:]
	}
}

// Rate returns the estimated bits/second. ok is false when the window
// holds too little recent data to trust.
func (e *Estimator) Rate(nowMs int64) (bps float64, ok bool) {
	if len(e.samples) == 0 {
		return 0, false
	}
	span := nowMs - e.samples[0].atMs
	if span < e.WindowMs/4 || nowMs-e.samples[len(e.samples)-1].atMs > e.WindowMs {
		return 0, false // estimate stale or too thin
	}
	return float64(e.total) * 8 * 1000 / float64(span), true
}

// Buffer computes adaptive jitter-buffer targets in milliseconds.
type Buffer struct {
	MinMs, MaxMs int64 // hard bounds
	FallbackMs   int64 // bounded conservative target when estimate is invalid
	RefRateBps   float64

	est           *Estimator
	history       []ratePoint
	AudioPriority bool // true after a sudden bandwidth drop
}

type ratePoint struct {
	atMs int64
	rate float64
}

func NewBuffer(est *Estimator, minMs, maxMs, fallbackMs int64, refRate float64) *Buffer {
	return &Buffer{MinMs: minMs, MaxMs: maxMs, FallbackMs: fallbackMs,
		RefRateBps: refRate, est: est}
}

func clamp(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Update ingests a reception sample and returns the current buffer
// target in milliseconds.
func (b *Buffer) Update(nowMs, bytes int64) int64 {
	b.est.Add(nowMs, bytes)
	rate, ok := b.est.Rate(nowMs)
	if !ok {
		return clamp(b.FallbackMs, b.MinMs, b.MaxMs) // bounded conservative
	}
	// Compare against the baseline one window ago: a halving inside a
	// single window is a sudden drop, not estimator drift.
	var base *ratePoint
	for i := range b.history {
		if b.history[i].atMs <= nowMs-b.est.WindowMs {
			base = &b.history[i]
		}
	}
	if base != nil {
		switch {
		case rate < base.rate/2:
			b.AudioPriority = true // sudden drop: protect audio clock
		case rate >= base.rate:
			b.AudioPriority = false
		}
	}
	b.history = append(b.history, ratePoint{nowMs, rate})
	cut := nowMs - 2*b.est.WindowMs
	for len(b.history) > 0 && b.history[0].atMs < cut {
		b.history = b.history[1:]
	}
	// Lower bandwidth -> deeper buffer, scaled around the fallback.
	target := int64(float64(b.FallbackMs) * b.RefRateBps / rate)
	return clamp(target, b.MinMs, b.MaxMs)
}

// Target returns the target for a known rate without feeding a sample.
func (b *Buffer) Target(rate float64, ok bool) int64 {
	if !ok || rate <= 0 {
		return clamp(b.FallbackMs, b.MinMs, b.MaxMs)
	}
	return clamp(int64(float64(b.FallbackMs)*b.RefRateBps/rate), b.MinMs, b.MaxMs)
}
