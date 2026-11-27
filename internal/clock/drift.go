package clock

import (
	"fmt"
	"sort"

	"avpipe/internal/media"
)

// DriftClass labels why a drift monitor produced a decision.
type DriftClass int

const (
	// DriftNone means every observation is inside tolerance.
	DriftNone DriftClass = iota
	// DriftExceeded means the subject clock wandered beyond tolerance from
	// the reference. The pipeline cuts a segment and records the reason.
	DriftExceeded
	// DriftJump means the reference clock itself made a discontinuous step
	// (a forward gap far outside its recent cadence). The operator must not
	// trust the surrounding timestamps.
	DriftJump
)

// String renders the stable token recorded in segment manifests.
func (d DriftClass) String() string {
	switch d {
	case DriftExceeded:
		return "drift_exceeded"
	case DriftJump:
		return "clock_jump"
	default:
		return "none"
	}
}

// DriftConfig bounds how much a clock may wander before a segment is cut.
type DriftConfig struct {
	// Tolerance is the maximum allowed offset between subject and reference
	// clocks, in seconds.
	Tolerance media.Rational
	// JumpFactor multiplies the recent median step to decide that a forward
	// reference gap is a jump rather than legitimate spacing. A factor of 0
	// disables jump detection.
	JumpFactor int64
	// Window is how many recent reference steps form the cadence baseline.
	Window int
}

// DefaultDriftConfig tolerates 50ms of drift and treats a gap of 8x the
// recent median step as a clock jump.
func DefaultDriftConfig() DriftConfig {
	tol, err := media.ParseDuration("50ms")
	if err != nil {
		panic(err)
	}
	return DriftConfig{Tolerance: tol, JumpFactor: 8, Window: 64}
}

// Validate checks the configuration and fills defaults.
func (c DriftConfig) Validate() (DriftConfig, error) {
	if c.Tolerance.Num < 0 || !c.Tolerance.Valid() {
		return c, fmt.Errorf("clock: invalid drift tolerance %s", c.Tolerance)
	}
	if c.Window <= 0 {
		c.Window = 64
	}
	if c.JumpFactor < 0 {
		c.JumpFactor = 0
	}
	return c, nil
}

// Observation is one resolved pair of reference and subject timestamps.
type Observation struct {
	Reference media.Time
	Subject   media.Time
	Origin    media.Origin
}

// Decision is the monitor's verdict for an observation.
type Decision struct {
	Class   DriftClass
	Offset  media.Rational // subject - reference, in seconds
	Baseline media.Rational
	Deviation media.Rational // absolute distance from the baseline
	Reason  string
	Origin  media.Origin
}

// Monitor tracks the offset between a reference clock and a subject clock and
// reports when the subject drifts too far or the reference jumps.
type Monitor struct {
	cfg      DriftConfig
	started  bool
	baseline media.Rational
	lastRef  media.Time
	lastOff  media.Rational
	steps    []media.Rational
	stepIdx  int
	maxOff   media.Rational
	events   []Decision
}

// NewMonitor builds a drift monitor.
func NewMonitor(cfg DriftConfig) (*Monitor, error) {
	c, err := cfg.Validate()
	if err != nil {
		return nil, media.Errorf(media.KindUsage, media.Origin{}, "clock.NewMonitor", "%v", err)
	}
	return &Monitor{cfg: c, stepIdx: -1}, nil
}

// Observe records one pair of timestamps and returns the resulting decision.
// The observation is always recorded so audits can replay the decision.
func (m *Monitor) Observe(o Observation) (Decision, error) {
	off, err := o.Subject.Sub(o.Reference)
	if err != nil {
		return Decision{}, media.New(media.KindInternal, o.Origin, "clock.Observe", "computing clock offset failed", err)
	}
	if !m.started {
		m.started = true
		m.baseline = off
		m.lastRef = o.Reference
		m.lastOff = off
		m.maxOff = media.MustRat(0, 1)
		d := Decision{Class: DriftNone, Offset: off, Baseline: m.baseline,
			Deviation: media.MustRat(0, 1), Origin: o.Origin}
		m.events = append(m.events, d)
		return d, nil
	}

	// Reference cadence: detect an implausible forward jump first, because a
	// reference jump makes the subject offset meaningless.
	if jump, reason := m.referenceJump(o.Reference); jump {
		d := Decision{Class: DriftJump, Offset: off, Baseline: m.baseline,
			Deviation: m.absDiff(off, m.baseline), Reason: reason, Origin: o.Origin}
		m.lastRef = o.Reference
		m.lastOff = off
		m.events = append(m.events, d)
		return d, nil
	}

	dev := m.absDiff(off, m.baseline)
	if m.maxOff.Cmp(dev) < 0 {
		m.maxOff = dev
	}
	d := Decision{Class: DriftNone, Offset: off, Baseline: m.baseline, Deviation: dev, Origin: o.Origin}
	if m.cfg.Tolerance.Cmp(dev) < 0 {
		d.Class = DriftExceeded
		d.Reason = fmt.Sprintf("subject clock drifted %s from baseline (tolerance %s)",
			dev.SecondsString(), m.cfg.Tolerance.SecondsString())
	}
	m.lastRef = o.Reference
	m.lastOff = off
	m.events = append(m.events, d)
	return d, nil
}

// referenceJump checks the step between the current and previous reference
// timestamp against the median of recent steps.
func (m *Monitor) referenceJump(ref media.Time) (bool, string) {
	step, err := ref.Sub(m.lastRef)
	if err != nil {
		return false, ""
	}
	recorded := m.recordStep(step)
	if m.cfg.JumpFactor <= 0 || !recorded {
		return false, ""
	}
	median := m.medianStep()
	if median.Num == 0 {
		return false, ""
	}
	limit, err := median.MulInt(m.cfg.JumpFactor)
	if err != nil {
		return false, ""
	}
	if step.Cmp(limit) > 0 {
		return true, fmt.Sprintf("reference clock jumped %s forward (recent median %s, limit %s)",
			step.SecondsString(), median.SecondsString(), limit.SecondsString())
	}
	return false, ""
}

func (m *Monitor) recordStep(step media.Rational) bool {
	if step.Num <= 0 {
		return false
	}
	if len(m.steps) < m.cfg.Window {
		m.steps = append(m.steps, step)
	} else {
		m.stepIdx = (m.stepIdx + 1) % m.cfg.Window
		m.steps[m.stepIdx] = step
	}
	return len(m.steps) >= 4
}

func (m *Monitor) medianStep() media.Rational {
	if len(m.steps) == 0 {
		return media.MustRat(0, 1)
	}
	cp := make([]media.Rational, len(m.steps))
	copy(cp, m.steps)
	sort.Slice(cp, func(i, j int) bool { return cp[i].Cmp(cp[j]) < 0 })
	mid := len(cp) / 2
	if len(cp)%2 == 1 {
		return cp[mid]
	}
	sum, err := cp[mid-1].Add(cp[mid])
	if err != nil {
		return cp[mid]
	}
	half, err := sum.Mul(media.MustRat(1, 2))
	if err != nil {
		return cp[mid]
	}
	return half
}

func (m *Monitor) absDiff(a, b media.Rational) media.Rational {
	d, err := a.Sub(b)
	if err != nil {
		return media.MustRat(0, 1)
	}
	if d.Num < 0 {
		return d.Neg()
	}
	return d
}

// Baseline returns the offset established by the first observation.
func (m *Monitor) Baseline() media.Rational { return m.baseline }

// MaxOffset returns the largest absolute deviation seen so far.
func (m *Monitor) MaxOffset() media.Rational { return m.maxOff }

// Events returns all recorded decisions.
func (m *Monitor) Events() []Decision { return m.events }

// SegmentsCut returns how many observations demanded a segment cut.
func (m *Monitor) SegmentsCut() int {
	n := 0
	for _, d := range m.events {
		if d.Class == DriftExceeded || d.Class == DriftJump {
			n++
		}
	}
	return n
}

// Reset anchors a new baseline, e.g. after a legitimate discontinuity.
func (m *Monitor) Reset() {
	m.started = false
	m.steps = nil
	m.stepIdx = -1
	m.events = nil
	m.maxOff = media.MustRat(0, 1)
}
