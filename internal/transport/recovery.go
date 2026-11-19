package transport

import (
	"sort"

	"mediamix/internal/core"
	"mediamix/internal/timeline"
)

// RecoverySource can hand back a packet that never arrived. In production this
// would be an RTX/NACK peer; in this local harness it is a seeded simulation
// that only returns packets the sender actually transmitted.
type RecoverySource interface {
	// Fetch returns a retransmitted copy of the packet at ext for the track.
	Fetch(track string, ext uint64) (Packet, bool)
}

// GapViability is optionally implemented by a RecoverySource that knows the
// current uplink budget. A gap that cannot fit on the link is unrecoverable
// regardless of how many retries are attempted, which is the honest outcome on
// a degraded connection.
type GapViability interface {
	GapViable(track string, startExt uint64, count int) bool
}

// RecoveryReport summarizes what happened while repairing one track.
type RecoveryReport struct {
	Track         string  `json:"track"`
	Attempted     int     `json:"attempted_gaps"`
	Recovered     int     `json:"recovered_gaps"`
	Unrecoverable int     `json:"unrecoverable_gaps"`
	Timeouts      int     `json:"timeouts"`
	AbandonedPkts int     `json:"abandoned_packets"`
	BufferBound   int     `json:"buffer_bound"`
	MaxDelayMs    float64 `json:"max_delay_ms"`
}

// Outcome classifies a repair attempt.
type Outcome int

const (
	// NoGap means there was nothing to repair.
	NoGap Outcome = iota
	// Healed means the gap was fully restored and delivery resumed.
	Healed
	// Blocked means the gap is still within its deadline and may yet heal.
	Blocked
	// Unrecoverable means the gap can never be restored; the caller must
	// advance past it.
	Unrecoverable
)

// RecoveryPolicy bounds how hard a single weak track may delay the pipeline.
type RecoveryPolicy struct {
	// MaxRetries is the number of retransmit attempts per gap.
	MaxRetries int
	// DeadlineMs is how long a gap may wait before it is declared a timeout.
	DeadlineMs float64
	// BandwidthMBps is the observed uplink sample.
	BandwidthMBps float64
	// MaxDelayMs caps playout delay.
	MaxDelayMs float64
}

// DefaultRecoveryPolicy is tuned for a 3 Mbps proxy feed.
func DefaultRecoveryPolicy() RecoveryPolicy {
	return RecoveryPolicy{MaxRetries: 3, DeadlineMs: 250, BandwidthMBps: 3, MaxDelayMs: 500}
}

// RecoveryEngine drives retransmit simulation and decides when a gap must be
// abandoned. It never blocks: every call returns immediately with whatever is
// currently recoverable.
type RecoveryEngine struct {
	policy    RecoveryPolicy
	ctrl      *BandwidthController
	src       RecoverySource
	reports   map[string]*RecoveryReport
	order     []string
	attempted map[string]uint64 // track -> gap start already attempted
}

// NewRecoveryEngine wires a recovery source and bandwidth policy.
func NewRecoveryEngine(policy RecoveryPolicy, src RecoverySource) *RecoveryEngine {
	if policy.MaxRetries <= 0 {
		policy.MaxRetries = 3
	}
	if policy.DeadlineMs <= 0 {
		policy.DeadlineMs = 250
	}
	if policy.BandwidthMBps <= 0 {
		policy.BandwidthMBps = 1
	}
	ctrl := NewController()
	if policy.MaxDelayMs > 0 {
		ctrl.MaxDelayMs = policy.MaxDelayMs
	}
	return &RecoveryEngine{
		policy:    policy,
		ctrl:      ctrl,
		src:       src,
		reports:   make(map[string]*RecoveryReport),
		attempted: make(map[string]uint64),
	}
}

// Policy returns the active policy.
func (e *RecoveryEngine) Policy() RecoveryPolicy { return e.policy }

// BufferBound returns the current per-track packet bound implied by bandwidth.
func (e *RecoveryEngine) BufferBound() int { return e.ctrl.MaxBuffered(e.policy.BandwidthMBps) }

// SetBandwidth updates the bandwidth sample and therefore the buffer bound.
func (e *RecoveryEngine) SetBandwidth(mbps float64) {
	if mbps > 0 {
		e.policy.BandwidthMBps = mbps
	}
}

// report returns (and lazily creates) a track's report.
func (e *RecoveryEngine) report(track string) *RecoveryReport {
	r, ok := e.reports[track]
	if !ok {
		r = &RecoveryReport{Track: track}
		e.reports[track] = r
		e.order = append(e.order, track)
	}
	return r
}

// RepairGap attempts the single open gap for a track. It is idempotent for one
// gap: repeated calls before the gap heals or is abandoned do not inflate the
// counters.
//
// A gap is healed only if every packet in it can be retransmitted. A partially
// repaired gap is not consumed: the caller either keeps waiting or advances
// past it, so a consumer never sees a half-restored span.
func (e *RecoveryEngine) RepairGap(buf *ReorderBuffer, now timeline.Micros) ([]Packet, Outcome, *RecoveryReport) {
	if buf == nil {
		return nil, Unrecoverable, nil
	}
	rep := e.report(buf.Track())
	rep.BufferBound = e.BufferBound()
	rep.MaxDelayMs = e.ctrl.MaxDelayMs

	g, ok := buf.OpenGap()
	if !ok {
		return nil, NoGap, rep
	}
	if prev, done := e.attempted[buf.Track()]; done && prev == g.StartExt {
		// Already tried this exact gap; only the deadline can change things.
		if buf.GapAgeMs(now) >= e.policy.DeadlineMs {
			return nil, Unrecoverable, rep
		}
		return nil, Blocked, rep
	}
	e.attempted[buf.Track()] = g.StartExt
	rep.Attempted++

	viable, _ := e.src.(GapViability)
	if e.src == nil || (viable != nil && !viable.GapViable(buf.Track(), g.StartExt, g.Count)) {
		rep.Unrecoverable++
		rep.AbandonedPkts += g.Count
		return nil, Unrecoverable, rep
	}

	got := make([]Packet, 0, g.Count)
	for ext := g.StartExt; ext <= g.EndExt; ext++ {
		var p Packet
		fetched := false
		for attempt := 0; attempt < e.policy.MaxRetries && !fetched; attempt++ {
			p, fetched = e.src.Fetch(buf.Track(), ext)
		}
		if !fetched {
			rep.Unrecoverable++
			rep.AbandonedPkts += g.Count
			if buf.GapAgeMs(now) >= e.policy.DeadlineMs {
				rep.Timeouts++
			}
			return nil, Unrecoverable, rep
		}
		p.Ext = ext
		p.Retransmit = true
		got = append(got, p)
	}
	rep.Recovered++
	var recovered []Packet
	for _, p := range got {
		recovered = append(recovered, buf.Insert(p)...)
	}
	sort.Slice(recovered, func(i, j int) bool { return recovered[i].Ext < recovered[j].Ext })
	return recovered, Healed, rep
}

// Report returns the accumulated report for a track.
func (e *RecoveryEngine) Report(track string) *RecoveryReport {
	if r, ok := e.reports[track]; ok {
		return r
	}
	return e.report(track)
}

// Reports returns all track reports in first-seen order.
func (e *RecoveryEngine) Reports() []*RecoveryReport {
	out := make([]*RecoveryReport, 0, len(e.order))
	for _, t := range e.order {
		out = append(out, e.reports[t])
	}
	return out
}

// RecoveryRate returns recovered/attempted across all tracks, or 1 when no
// gaps were ever attempted.
func (e *RecoveryEngine) RecoveryRate() float64 {
	attempted, recovered := 0, 0
	for _, r := range e.reports {
		attempted += r.Attempted
		recovered += r.Recovered
	}
	if attempted == 0 {
		return 1
	}
	return float64(recovered) / float64(attempted)
}

// Unrecoverable reports whether any track permanently lost media.
func (e *RecoveryEngine) HasUnrecoverable() bool {
	for _, r := range e.reports {
		if r.Unrecoverable > 0 {
			return true
		}
	}
	return false
}

// Ensure core is referenced even if future edits drop other uses.
var _ = core.CodeTimeout
