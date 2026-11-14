// Package relay implements RTP reordering, deduplication, forwarding and
// metrics. It consumes RTP packets only and has no dependency on signalling or
// PeerConnection internals.
package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// ErrInvalidReport marks unreadable or structurally invalid report input.
var ErrInvalidReport = errors.New("relay: invalid report")

// JitterQuantiles contains the distribution requested by relayctl reports.
type JitterQuantiles struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	Max float64 `json:"max"`
}

// Metrics is the stable JSON counter snapshot. Received counts first-time
// sequence arrivals; Unique counts those admitted into the jitter window. Thus
// duplicates are accounted separately and forwarded always equals unique.
type Metrics struct {
	Emitted       uint64          `json:"emitted"`
	Received      uint64          `json:"received"`
	Unique        uint64          `json:"unique"`
	Forwarded     uint64          `json:"forwarded"`
	Duplicates    uint64          `json:"duplicates"`
	Reordered     uint64          `json:"reordered"`
	LateDropped   uint64          `json:"late_dropped"`
	LossRatio     float64         `json:"loss_ratio"`
	ForwardRatio  float64         `json:"forward_ratio"`
	ForwardBytes  uint64          `json:"forward_bytes"`
	ForwardErrors uint64          `json:"forward_errors"`
	JitterMS      JitterQuantiles `json:"jitter_ms"`
}

// Stats is safe for concurrent packet and reporter use. All counters are
// atomic; jitter samples use a short mutex because percentiles need a snapshot.
type Stats struct {
	emitted     atomic.Uint64
	received    atomic.Uint64
	unique      atomic.Uint64
	forwarded   atomic.Uint64
	duplicates  atomic.Uint64
	reordered   atomic.Uint64
	lateDropped atomic.Uint64
	bytes       atomic.Uint64
	errors      atomic.Uint64

	jitterMu sync.Mutex
	jitter   []float64
}

// NewStats creates an empty counter set.
func NewStats() *Stats { return &Stats{} }

func (s *Stats) SetEmitted(n uint64)    { s.emitted.Store(n) }
func (s *Stats) IncReceived()           { s.received.Add(1) }
func (s *Stats) IncUnique()             { s.unique.Add(1) }
func (s *Stats) IncDuplicate()          { s.duplicates.Add(1) }
func (s *Stats) IncReordered()          { s.reordered.Add(1) }
func (s *Stats) IncLateDropped()        { s.lateDropped.Add(1) }
func (s *Stats) IncForwarded(bytes int) { s.forwarded.Add(1); s.bytes.Add(uint64(bytes)) }
func (s *Stats) IncForwardError()       { s.errors.Add(1) }

// RecordJitter stores one sampled RFC 3550 jitter value in milliseconds.
func (s *Stats) RecordJitter(ms float64) {
	if math.IsNaN(ms) || math.IsInf(ms, 0) || ms < 0 {
		return
	}
	s.jitterMu.Lock()
	s.jitter = append(s.jitter, ms)
	s.jitterMu.Unlock()
}

// Snapshot returns a coherent-enough JSON view. Ratios and percentiles are
// computed from the atomic counters and a copied sample slice.
func (s *Stats) Snapshot() Metrics {
	m := Metrics{
		Emitted:       s.emitted.Load(),
		Received:      s.received.Load(),
		Unique:        s.unique.Load(),
		Forwarded:     s.forwarded.Load(),
		Duplicates:    s.duplicates.Load(),
		Reordered:     s.reordered.Load(),
		LateDropped:   s.lateDropped.Load(),
		ForwardBytes:  s.bytes.Load(),
		ForwardErrors: s.errors.Load(),
	}
	if m.Emitted > 0 {
		missing := uint64(0)
		if m.Emitted > m.Unique {
			missing = m.Emitted - m.Unique
		}
		m.LossRatio = float64(missing) / float64(m.Emitted)
		m.ForwardRatio = float64(m.Forwarded) / float64(m.Emitted)
	}
	s.jitterMu.Lock()
	samples := append([]float64(nil), s.jitter...)
	s.jitterMu.Unlock()
	m.JitterMS = SummarizeJitter(samples)
	return m
}

// SummarizeJitter calculates nearest-rank p50/p95 and max in milliseconds.
func SummarizeJitter(samples []float64) JitterQuantiles {
	if len(samples) == 0 {
		return JitterQuantiles{}
	}
	sort.Float64s(samples)
	return JitterQuantiles{
		P50: Quantile(samples, 0.50),
		P95: Quantile(samples, 0.95),
		Max: samples[len(samples)-1],
	}
}

// Quantile returns a nearest-rank percentile for sorted samples.
func Quantile(sortedSamples []float64, p float64) float64 {
	if len(sortedSamples) == 0 {
		return 0
	}
	if p <= 0 {
		return sortedSamples[0]
	}
	if p >= 1 {
		return sortedSamples[len(sortedSamples)-1]
	}
	rank := int(math.Ceil(p*float64(len(sortedSamples)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sortedSamples) {
		rank = len(sortedSamples) - 1
	}
	return sortedSamples[rank]
}

// Report is written by demo/probe and read by verify.
type Report struct {
	Schema           string    `json:"schema"`
	GeneratedAt      time.Time `json:"generated_at"`
	Seed             int64     `json:"seed"`
	PacketsRequested int       `json:"packets_requested"`
	DurationMS       int64     `json:"duration_ms"`
	ClockRate        uint32    `json:"clock_rate"`
	StartSequence    uint16    `json:"start_sequence"`
	SourceSequence   uint16    `json:"source_start_sequence"`
	ForwardSSRC      uint32    `json:"forward_ssrc"`
	Sink             string    `json:"sink,omitempty"`
	SinkPackets      uint64    `json:"sink_packets,omitempty"`
	SinkBytes        uint64    `json:"sink_bytes,omitempty"`
	SelectedPair     string    `json:"selected_pair,omitempty"`
	BannedCandidates uint64    `json:"banned_candidates"`
	Metrics
}

// NewReport fills stable metadata while leaving counters from Stats.
func NewReport(seed int64, packets int, duration time.Duration, clockRate uint32, startSeq uint16, sink, selected string, banned uint64, metrics Metrics) Report {
	return Report{
		Schema:           "relayctl.report.v1",
		GeneratedAt:      time.Now().UTC(),
		Seed:             seed,
		PacketsRequested: packets,
		DurationMS:       duration.Milliseconds(),
		ClockRate:        clockRate,
		StartSequence:    startSeq,
		Sink:             sink,
		SelectedPair:     selected,
		BannedCandidates: banned,
		Metrics:          metrics,
	}
}

// WriteReport atomically writes indented JSON, creating parent directories.
func WriteReport(path string, report Report) error {
	if path == "" {
		return fmt.Errorf("empty report path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create report directory: %w", err)
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace report: %w", err)
	}
	return nil
}

// ReadReport reads and minimally validates a report.
func ReadReport(path string) (Report, error) {
	if path == "" {
		return Report{}, fmt.Errorf("%w: empty report path", ErrInvalidReport)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Report{}, fmt.Errorf("%w: %v", ErrInvalidReport, err)
	}
	var report Report
	if err := json.Unmarshal(data, &report); err != nil {
		return Report{}, fmt.Errorf("%w: %v", ErrInvalidReport, err)
	}
	if report.Emitted == 0 {
		return Report{}, fmt.Errorf("%w: emitted is zero", ErrInvalidReport)
	}
	if report.Unique > report.Emitted || report.Forwarded > report.Unique || report.Received < report.Unique {
		return Report{}, fmt.Errorf("%w: inconsistent packet counters", ErrInvalidReport)
	}
	if math.IsNaN(report.LossRatio) || math.IsInf(report.LossRatio, 0) || report.LossRatio < 0 || report.LossRatio > 1 {
		return Report{}, fmt.Errorf("%w: loss_ratio out of range", ErrInvalidReport)
	}
	if report.JitterMS.P50 < 0 || report.JitterMS.P95 < 0 || report.JitterMS.Max < 0 {
		return Report{}, fmt.Errorf("%w: negative jitter", ErrInvalidReport)
	}
	if report.JitterMS.P50 > report.JitterMS.P95 || report.JitterMS.P95 > report.JitterMS.Max {
		return Report{}, fmt.Errorf("%w: unordered jitter quantiles", ErrInvalidReport)
	}
	return report, nil
}
