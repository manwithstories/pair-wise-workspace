// Package recovery estimates available bandwidth with an exponential moving
// average and decides how many re-fetch attempts each gap is worth. A gap the
// budget cannot close is labelled explicit rather than silently stitched.
package recovery

import (
	"math"

	"pairwise/ingestcheck/internal/clock"
)

// Config tunes the bandwidth estimator and the re-fetch budget.
type Config struct {
	Alpha        float64 // EWMA weight for the newest bandwidth sample
	InitialMbps  float64 // optimistic starting estimate
	MinMbps      float64 // floor so a silent link still yields a budget
	MaxMbps      float64 // ceiling so a burst cannot inflate the budget
	BaseAttempts int     // attempts granted at or below the reference rate
	MaxAttempts  int     // hard cap on attempts per gap
	CostMs       int64   // time budget per re-fetch attempt
}

// DefaultConfig mirrors the sample's weak-link profile.
func DefaultConfig() Config {
	return Config{
		Alpha:        0.25,
		InitialMbps:  8.0,
		MinMbps:      0.25,
		MaxMbps:      64.0,
		BaseAttempts: 2,
		MaxAttempts:  6,
		CostMs:       120,
	}
}

// Meter tracks the EWMA bandwidth estimate.
type Meter struct {
	cfg   Config
	est   float64
	samps int
}

// NewMeter returns a meter seeded with the configured initial estimate.
func NewMeter(cfg Config) (*Meter, error) {
	if cfg.Alpha <= 0 || cfg.Alpha >= 1 {
		return nil, clock.New(clock.CodeArgument, "recovery", "alpha must lie in (0,1)")
	}
	if cfg.InitialMbps <= 0 || cfg.MinMbps <= 0 || cfg.MaxMbps < cfg.MinMbps {
		return nil, clock.New(clock.CodeArgument, "recovery", "invalid bandwidth bound")
	}
	if cfg.BaseAttempts <= 0 || cfg.MaxAttempts < cfg.BaseAttempts {
		return nil, clock.New(clock.CodeArgument, "recovery", "invalid attempt bound")
	}
	if cfg.CostMs <= 0 {
		return nil, clock.New(clock.CodeArgument, "recovery", "invalid cost bound")
	}
	return &Meter{cfg: cfg, est: cfg.InitialMbps}, nil
}

// Observe folds a new bandwidth sample into the estimate. Non-finite samples
// are ignored and finite samples are clamped so one corrupt measurement
// cannot pin the estimate.
func (m *Meter) Observe(mbps float64) {
	if math.IsNaN(mbps) || math.IsInf(mbps, 0) {
		return
	}
	if mbps < m.cfg.MinMbps {
		mbps = m.cfg.MinMbps
	}
	if mbps > m.cfg.MaxMbps {
		mbps = m.cfg.MaxMbps
	}
	if m.samps == 0 {
		m.est = mbps
	} else {
		m.est = m.cfg.Alpha*mbps + (1-m.cfg.Alpha)*m.est
	}
	m.samps++
}

// Estimate returns the current EWMA bandwidth in Mbps.
func (m *Meter) Estimate() float64 { return m.est }

// Plan is the bounded re-fetch decision for one gap.
type Plan struct {
	Attempts int
	BudgetMs int64
	Estimate float64
	// JitterMs is the reorder wait derived from the estimate: slower links
	// get a wider window but never wider than MaxWaitMs.
	JitterMs int64
}

// MaxWaitMs caps both the jitter wait and the re-fetch budget so the total
// latency stays bounded no matter how far bandwidth falls.
const MaxWaitMs int64 = 1000

// Decide converts a gap into a bounded re-fetch plan. Attempts grow with the
// amount of missing data but are capped by MaxAttempts and by the wall-clock
// budget implied by CostMs.
func (m *Meter) Decide(missingBytes int64) Plan {
	if missingBytes < 0 {
		missingBytes = 0
	}
	bits := float64(missingBytes) * 8
	seconds := bits / (m.est * 1e6)
	if seconds <= 0 {
		seconds = 0.001
	}
	raw := int(math.Ceil(seconds*1000/float64(m.cfg.CostMs))) + m.cfg.BaseAttempts
	if raw < 1 {
		raw = 1
	}
	attempts := raw
	if attempts > m.cfg.MaxAttempts {
		attempts = m.cfg.MaxAttempts
	}
	jitter := int64(seconds * 1000)
	if jitter > MaxWaitMs {
		jitter = MaxWaitMs
	}
	return Plan{
		Attempts: attempts,
		BudgetMs: int64(attempts) * m.cfg.CostMs,
		Estimate: m.est,
		JitterMs: jitter,
	}
}

// MaxWait returns the jitter wait, bounded.
func (p Plan) MaxWait() int64 {
	if p.JitterMs > MaxWaitMs {
		return MaxWaitMs
	}
	return p.JitterMs
}

// Outcome labels what the budget produced.
type Outcome string

const (
	Recovered   Outcome = "recovered"
	ExplicitGap Outcome = "explicit_gap"
)

// Gap decides and runs the bounded re-fetch loop for one hole. Fetch is the
// caller-supplied attempt; the loop tries exactly Plan.Attempts times at most
// and, if all fail, returns ExplicitGap without stitching.
func (m *Meter) Gap(missingBytes int64, missingFragments int) (Outcome, Plan, int) {
	// Header jumps beyond a small threshold are not chased at all: the
	// history is too far gone to be recoverable within the budget.
	if missingFragments > 4 {
		return ExplicitGap, m.Decide(missingBytes), 0
	}
	plan := m.Decide(missingBytes)
	for i := 1; i <= plan.Attempts; i++ {
		// A re-fetch yields the missing bytes when one attempt's wall clock
		// fits inside the measured budget at the current estimate.
		capacity := plan.Estimate * float64(m.cfg.CostMs) * 1000 / 8 // bytes per attempt
		if float64(missingBytes) <= capacity*float64(i) {
			return Recovered, plan, i
		}
	}
	return ExplicitGap, plan, plan.Attempts
}
