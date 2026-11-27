// Package bitrate estimates available bandwidth from observed samples and
// selects representation bitrates with hysteresis. Sharp drops throttle
// immediately; sharp rises are ignored until they persist, so a noisy link
// cannot trigger representation flapping.
package bitrate

import (
	"fmt"
	"sort"

	"archive/internal/model"
)

// Representation is one selectable encoding, ordered by bitrate.
type Representation struct {
	Name  string
	Kbps  int
}

// Estimator maintains an exponentially weighted moving average of the
// offered bandwidth and classifies the direction and sharpness of change.
type Estimator struct {
	alpha         float64 // EWMA weight of a new sample
	dropRatio     float64 // sample below dropRatio*ewma counts as a sharp drop
	riseRatio     float64 // sample above riseRatio*ewma counts as a sharp rise
	persistRise   int     // consecutive rises required before upgrading
	switchDownAt  float64 // downgrade when ewma < switchDownAt*target
	switchUpAt    float64 // upgrade only when ewma > switchUpAt*target
	log           *model.FaultLog
	ewma          float64
	primed        bool
	riseRun       int
	throttled     bool
	targetKbps    int
}

// NewEstimator builds an estimator for the configured target bitrate. The
// thresholds are fixed archive-safe defaults rather than caller-tunable
// magic, keeping recovery behaviour reproducible.
func NewEstimator(targetKbps int, log *model.FaultLog) *Estimator {
	return &Estimator{
		alpha:        0.4,
		dropRatio:    0.6,
		riseRatio:    1.6,
		persistRise:  3,
		switchDownAt: 0.9,
		switchUpAt:   1.15,
		log:          log,
		targetKbps:   targetKbps,
	}
}

// Observe feeds one bandwidth sample in kbps and returns whether the sample
// represents a sharp drop or a *persistent* rise relative to the estimate.
// A single high sample reports sharpRise=false: upgrades require the rise
// to survive persistRise observations, which keeps representation flapping
// off the table during bursty links.
func (e *Estimator) Observe(kbps float64) (sharpDrop, persistentRise bool) {
	if !e.primed {
		e.primed = true
		e.ewma = kbps
		return false, false
	}
	sharpRise := kbps > e.riseRatio*e.ewma
	if kbps < e.dropRatio*e.ewma {
		sharpDrop = true
		e.throttled = true
		e.riseRun = 0
		e.log.Add(model.Fault{Phase: model.PhaseBitrate, Kind: model.FaultBackpressure,
			Detail: fmt.Sprintf("sharp bandwidth drop: sample %.0f vs ewma %.0f kbps", kbps, e.ewma)})
	} else if kbps > e.ewma && !sharpRise {
		// Only steady upward movement extends the run. A single abrupt
		// spike (sharpRise) does not extend it, so one good sample after a
		// drop can never be mistaken for a persistent rise.
		e.riseRun++
	} else if kbps <= e.ewma {
		e.riseRun = 0
	}
	e.ewma = e.alpha*kbps + (1-e.alpha)*e.ewma
	if e.throttled && e.ewma >= float64(e.targetKbps)*e.switchUpAt {
		e.throttled = false // bandwidth has recovered past the safety margin
	}
	persistentRise = e.riseRun >= e.persistRise &&
		e.ewma >= float64(e.targetKbps)*e.switchUpAt
	return sharpDrop, persistentRise
}

// EwmaKbps returns the current smoothed bandwidth estimate.
func (e *Estimator) EwmaKbps() float64 { return e.ewma }

// Throttled reports whether the estimator is currently rate-limiting after
// an unhealed sharp drop.
func (e *Estimator) Throttled() bool { return e.throttled }

// BufferTargetNanos maps bandwidth pressure to a jitter-buffer target. Low
// bandwidth demands a deeper buffer to absorb further stalls; comfortable
// bandwidth shrinks it to cut latency. Bounds keep the value inside the
// repair window budget.
func (e *Estimator) BufferTargetNanos(base model.Nano) model.Nano {
	switch {
	case !e.primed:
		return base
	case e.throttled || e.ewma < float64(e.targetKbps)*e.switchDownAt:
		return clampNano(base*2, base, model.Nano(model.MaxRepairWindow)*model.Millisecond*100)
	case e.ewma > float64(e.targetKbps)*e.switchUpAt:
		return clampNano(base/2, base/4, base)
	default:
		return base
	}
}

// Selector picks representations with hysteresis: downgrades are immediate
// when the estimate falls below 90% of the current representation, upgrades
// require the estimate to clear 115% of the next representation, a
// persistent rise signal and a cooldown after the previous change.
type Selector struct {
	reps        []Representation
	cur         int
	estim       *Estimator
	log         *model.FaultLog
	cooldown    int // observations to wait after the last switch
	sinceSwitch int
}

// NewSelector builds a selector over representations sorted ascending by
// bitrate. The starting choice is the highest representation the target
// bitrate can sustain.
func NewSelector(reps []Representation, estim *Estimator, log *model.FaultLog) *Selector {
	sorted := append([]Representation(nil), reps...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Kbps < sorted[j].Kbps })
	cur := 0
	for i, r := range sorted {
		if float64(r.Kbps) <= estim.targetKbpsFloat() {
			cur = i
		}
	}
	return &Selector{reps: sorted, cur: cur, estim: estim, log: log,
		cooldown: estim.persistRise, sinceSwitch: estim.persistRise}
}

// Choose returns the representation to use after absorbing one bandwidth
// observation. Downgrades never skip more than one step per call, so a
// collapse still degrades gracefully rather than jumping to the floor.
func (s *Selector) Choose(sharpDrop, persistentRise bool) Representation {
	s.sinceSwitch++
	ewma := s.estim.EwmaKbps()
	if sharpDrop || ewma < float64(s.reps[s.cur].Kbps)*s.estim.switchDownAt {
		if s.cur > 0 {
			s.cur--
			s.sinceSwitch = 0
		}
	} else if persistentRise && s.sinceSwitch >= s.cooldown && s.cur < len(s.reps)-1 {
		next := s.reps[s.cur+1]
		if ewma >= float64(next.Kbps)*s.estim.switchUpAt {
			s.cur++
			s.sinceSwitch = 0
		}
	}
	return s.reps[s.cur]
}

// Current returns the selected representation without changing it.
func (s *Selector) Current() Representation { return s.reps[s.cur] }

func (e *Estimator) targetKbpsFloat() float64 { return float64(e.targetKbps) }

func clampNano(v, lo, hi model.Nano) model.Nano {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
