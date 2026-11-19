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

// RecoveryReport summarizes what happened while repairing one track.
type RecoveryReport struct {
	Track        string          `json:"track"`
	Attempted    int             `json:"attempted"`
	Recovered    int             `json:"recovered"`
	Unrecoverable int            `json:"unrecoverable"`
	Timeouts     int             `json:"timeouts"`
	Affected     []timeline.Span `json:"affected_us,omitempty"`
	BufferBound  int             `json:"buffer_bound"`
	MaxDelayMs   float64         `json:"max_delay_ms"`
}

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
	policy  RecoveryPolicy
	ctrl    *BandwidthController
	src     RecoverySource
	reports map[string]*RecoveryReport
	order   []string
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
		policy:  policy,
		ctrl:    ctrl,
		src:     src,
		reports: make(map[string]*RecoveryReport),
	}
}

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

// Recover processes a track's open gaps. It returns recovered packets in
// sequence order, and the gaps that are now abandoned with their affected
// time spans. A gap that cannot be repaired never blocks other tracks.
func (e *RecoveryEngine) Recover(buf *ReorderBuffer, mkSpan func(ext uint64) timeline.Span, now timeline.Micros) ([]Packet, *RecoveryReport, error) {
	if buf == nil {
		return nil, nil, &core.Error{Code: core.CodeInternal, Detail: "nil reorder buffer", Fatal: true}
	}
	rep := e.report(buf.Track())
	rep.BufferBound = e.BufferBound()
	rep.MaxDelayMs = e.ctrl.MaxDelayMs

	gaps := buf.OpenGaps()
	if len(gaps) == 0 {
		return nil, rep, nil
	}
	var recovered []Packet
	var abandoned []Gap
	for _, g := range gaps {
		rep.Attempted++
		got := false
		if e.src != nil {
			for ext := g.StartExt; ext <= g.EndExt; ext++ {
				if p, ok := e.src.Fetch(buf.Track(), ext); ok {
					p.Ext = ext
					p.Retransmit = true
					recovered = append(recovered, buf.Recovered(p)...)
					got = true
				}
			}
		}
		if got {
			rep.Recovered++
			continue
		}
		ageMs := float64(now-g.SinceUs) / 1000
		if ageMs >= e.policy.DeadlineMs {
			rep.Timeouts++
			rep.Unrecoverable++
			abandoned = append(abandoned, g)
		}
	}
	sort.Slice(recovered, func(i, j int) bool { return recovered[i].Ext < recovered[j].Ext })
	for _, g := range abandoned {
		// Only the sequence span is known here; the caller maps it onto media
		// time with codec keyframe boundaries.
		rep.Affected = append(rep.Affected, spanForGap(g, mkSpan))
	}
	return recovered, rep, nil
}

// spanForGap converts a sequence gap into a media span using the caller's
// mapping, falling back to a zero span when the mapping is unavailable.
func spanForGap(g Gap, mkSpan func(ext uint64) timeline.Span) timeline.Span {
	if mkSpan == nil {
		return timeline.Span{}
	}
	start := mkSpan(g.StartExt)
	end := mkSpan(g.EndExt + 1)
	if end.Start <= start.Start {
		// Single packet gap: derive an end from the next packet's start.
		end = timeline.Span{Start: start.End, End: start.End}
	}
	if !start.Valid() || end.Start < start.Start {
		return timeline.Span{}
	}
	return timeline.Span{Start: start.Start, End: maxMicros(end.Start, start.End)}
}

func maxMicros(a, b timeline.Micros) timeline.Micros {
	if a > b {
		return a
	}
	return b
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

// RecoveryRate returns recovered/(attempted) across all tracks, or 1 when no
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
