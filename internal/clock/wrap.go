// Package clock provides a unified media clock across tracks: timestamp
// unwrapping, drift estimation and a shared 90 kHz timeline.
package clock

// Unwrapper extends wrapping timestamps (e.g. 33-bit RTP capture clocks)
// into a monotonic 64-bit domain.
type Unwrapper struct {
	Modulus uint64 // wrap modulus; 0 disables unwrapping
	Wraps   int64  // number of forward wraps observed

	init bool
	last uint64
}

// NewUnwrapper returns an Unwrapper for the given modulus.
func NewUnwrapper(modulus uint64) *Unwrapper { return &Unwrapper{Modulus: modulus} }

// Unwrap maps a raw wrapping timestamp into the monotonic domain. Forward
// steps of at most half the modulus advance the clock (crossing the modulus
// boundary increments the cycle count); backward steps are treated as
// jitter and never move the cycle count backwards.
func (u *Unwrapper) Unwrap(raw uint64) int64 {
	if u.Modulus == 0 {
		return int64(raw)
	}
	raw %= u.Modulus
	if !u.init {
		u.init, u.last = true, raw
		return int64(raw)
	}
	fwd := (raw - u.last) % u.Modulus
	if fwd != 0 && fwd <= u.Modulus/2 && raw < u.last {
		u.Wraps++
	}
	u.last = raw
	return int64(u.Modulus)*u.Wraps + int64(raw)
}

// DriftEstimator measures clock drift in ppm between a reference clock
// (capture timestamps) and a media clock (tfdt-derived).
type DriftEstimator struct {
	ref0, med0  int64
	ref1, med1  int64
	count       int
	initialized bool
}

// Observe feeds a (reference, media) timestamp pair.
func (d *DriftEstimator) Observe(ref, med int64) {
	if !d.initialized {
		d.ref0, d.med0, d.initialized = ref, med, true
	}
	d.ref1, d.med1 = ref, med
	d.count++
}

// PPM returns the estimated drift of the media clock relative to the
// reference, in parts per million. Returns 0 with fewer than 2 samples.
func (d *DriftEstimator) PPM() float64 {
	if d.count < 2 {
		return 0
	}
	refDelta := d.ref1 - d.ref0
	if refDelta <= 0 {
		return 0
	}
	medDelta := d.med1 - d.med0
	return float64(medDelta-refDelta) * 1e6 / float64(refDelta)
}

// Count returns the number of observed pairs.
func (d *DriftEstimator) Count() int { return d.count }
