// Package media contains the shared time and error vocabulary used by the
// parsing, timeline and remux subsystems.
package media

import (
	"errors"
	"math"
	"math/big"
)

// Rational is an integer rational time base. Den must be positive.
type Rational struct {
	Num int64
	Den int64
}

// TicksPerSecond returns the number of clock ticks in one second.
func (r Rational) TicksPerSecond() int64 {
	if r.Den <= 0 || r.Num <= 0 {
		return 0
	}
	return r.Den / r.Num
}

// Scale converts v from one time base to another using round-to-nearest.
func Scale(v int64, from, to Rational) int64 {
	if from.Num == 0 || from.Den == 0 || to.Num == 0 || to.Den == 0 {
		return 0
	}
	num := new(big.Int).Mul(big.NewInt(v), big.NewInt(from.Num*to.Den))
	den := big.NewInt(from.Den * to.Num)
	q := new(big.Int).Quo(num, den)
	return q.Int64()
}

// Ms converts a tick value to rounded milliseconds.
func Ms(v int64, base Rational) int64 {
	return Scale(v, base, Rational{Num: 1, Den: 1000})
}

// WrapExtender expands unsigned wrapping counters into monotone int64 values.
// The zero value is not usable; use NewWrapExtender.
type WrapExtender struct {
	mod      int64
	half     int64
	lastRaw  int64
	lastFull int64
	have     bool
	Wraps    int
}

// NewPTSWrapExtender builds the conventional 33-bit MPEG PTS/DTS counter.
func NewPTSWrapExtender() *WrapExtender {
	return NewWrapExtender(1 << 33)
}

// NewWrapExtender builds an extender for a counter modulo mod.
func NewWrapExtender(mod int64) *WrapExtender {
	if mod <= 1 {
		mod = 1 << 33
	}
	return &WrapExtender{mod: mod, half: mod / 2}
}

// Extend maps raw to a monotone value close to the preceding sample.
func (w *WrapExtender) Extend(raw uint64) int64 {
	r := int64(raw % uint64(w.mod))
	if !w.have {
		w.have = true
		w.lastRaw = r
		w.lastFull = r
		return r
	}
	d := r - w.lastRaw
	if d > w.half {
		d -= w.mod
		w.Wraps++
	} else if d < -w.half {
		d += w.mod
	}
	w.lastFull += d
	w.lastRaw = r
	return w.lastFull
}

// DriftSample is one clock/reference observation.
type DriftSample struct {
	Local     int64
	Reference int64
}

// DriftEstimator is a bounded-window least-squares estimator. It retains at
// most Window observations, keeping memory bounded for long batch processes.
type DriftEstimator struct {
	Window  int
	samples []DriftSample
}

// NewDriftEstimator creates an estimator with a bounded ring.
func NewDriftEstimator(window int) *DriftEstimator {
	if window < 2 {
		window = 32
	}
	return &DriftEstimator{Window: window}
}

// Observe adds a clock/reference pair.
func (d *DriftEstimator) Observe(local, reference int64) {
	if len(d.samples) == d.Window {
		copy(d.samples, d.samples[1:])
		d.samples[len(d.samples)-1] = DriftSample{local, reference}
		return
	}
	d.samples = append(d.samples, DriftSample{local, reference})
}

// DriftPPM returns estimated local-clock error relative to reference.
func (d *DriftEstimator) DriftPPM() float64 {
	if len(d.samples) < 2 {
		return 0
	}
	n := float64(len(d.samples))
	var sx, sy, sxx, sxy float64
	for _, s := range d.samples {
		x := float64(s.Local)
		y := float64(s.Reference)
		sx += x
		sy += y
		sxx += x * x
		sxy += x * y
	}
	den := n*sxx - sx*sx
	if math.Abs(den) < 1e-9 {
		return 0
	}
	slope := (n*sxy - sx*sy) / den
	return (slope - 1) * 1_000_000
}

// Correct applies the estimated rate correction to a local tick value.
func (d *DriftEstimator) Correct(local int64) int64 {
	ppm := d.DriftPPM()
	if ppm == 0 {
		return local
	}
	return int64(math.Round(float64(local) * (1 - ppm/1_000_000)))
}

// ValidateRational rejects unusable time bases.
func ValidateRational(r Rational) error {
	if r.Num <= 0 || r.Den <= 0 {
		return errors.New("media: rational must have positive numerator and denominator")
	}
	return nil
}
