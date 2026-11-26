// Package report aggregates subsystem results into the single set of metrics
// the CLI emits and the acceptance gates evaluate.
package report

import (
	"sort"

	"mediaflow/internal/transport"
)

// Field names printed in the terminal output; kept as constants for stability.
const (
	FieldCase          = "case"
	FieldSyncMaxMS     = "sync_max_ms"
	FieldLost          = "lost"
	FieldRecovered     = "recovered"
	FieldRecoveryRate  = "recovery_rate"
	FieldP95MS         = "p95_ms"
	FieldIndex         = "index"
	FieldOutputShaMatch = "output_sha_match"
)

// Metrics is the per-case acceptance record.
type Metrics struct {
	Case            string  `json:"case"`
	SyncMaxMS       float64 `json:"sync_max_ms"`
	Lost            int     `json:"lost"`
	Recovered       int     `json:"recovered"`
	RecoveryRate    float64 `json:"recovery_rate"`
	P95MS           float64 `json:"p95_ms"`
	Index           string  `json:"index"`
	OutputShaMatch  bool    `json:"output_sha_match"`
	// Supporting detail not part of the required headline fields.
	Gaps          int `json:"gaps"`
	Truncated     int `json:"truncated"`
	Duplicates    int `json:"duplicates"`
	Reordered     int `json:"reordered"`
	FramesWritten int `json:"frames_written"`
}

// Thresholds encode the acceptance gates.
type Thresholds struct {
	SyncMaxMS       float64
	RecoveryRate    float64
	P95MS           float64
}

// DefaultThresholds matches the acceptance criteria: 20 ms sync, 99% recovery,
// 250 ms single-segment P95.
func DefaultThresholds() Thresholds {
	return Thresholds{SyncMaxMS: 20.0, RecoveryRate: 0.99, P95MS: 250.0}
}

// Inputs carries the raw measurements to aggregate.
type Inputs struct {
	Case            string
	SyncResidualUS  int64
	Transport       transport.Stats
	P95SegmentUS    int64
	IndexHash       string
	OutputShaMatch  bool
	TruncatedEvents int
}

// Build computes the headline metrics from subsystem measurements.
func Build(in Inputs) Metrics {
	m := Metrics{
		Case:           in.Case,
		SyncMaxMS:      microsToMillis(in.SyncResidualUS),
		Lost:           in.Transport.LostMedia,
		Recovered:      in.Transport.Recovered,
		P95MS:          microsToMillis(in.P95SegmentUS),
		Index:          in.IndexHash,
		OutputShaMatch: in.OutputShaMatch,
		Gaps:           in.Transport.Gaps,
		Truncated:      in.TruncatedEvents,
		Duplicates:     in.Transport.Duplicates,
		Reordered:      in.Transport.Reordered,
	}
	if m.Lost > 0 {
		m.RecoveryRate = float64(m.Recovered) / float64(m.Lost)
	} else {
		// No loss means perfect recovery by definition.
		m.RecoveryRate = 1.0
	}
	return m
}

// GateResult explains which acceptance gates a metrics record passes.
type GateResult struct {
	Pass     bool
	Failures []string
}

// Evaluate applies the thresholds. The clean and resume cases must satisfy all
// gates; the damage case is allowed unrecovered gaps but still reports them.
func (m Metrics) Evaluate(th Thresholds, allowGaps bool) GateResult {
	var failures []string
	if m.SyncMaxMS > th.SyncMaxMS {
		failures = append(failures, "sync_max_ms")
	}
	if !allowGaps && m.RecoveryRate < th.RecoveryRate {
		failures = append(failures, "recovery_rate")
	}
	if m.P95MS > th.P95MS {
		failures = append(failures, "p95_ms")
	}
	if !m.OutputShaMatch {
		failures = append(failures, "output_sha_match")
	}
	return GateResult{Pass: len(failures) == 0, Failures: failures}
}

// Row renders metrics as a stable, machine-parseable key=value line.
func (m Metrics) Row() string {
	return joinFields([]field{
		{FieldCase, m.Case},
		{FieldSyncMaxMS, formatFloat(m.SyncMaxMS)},
		{FieldLost, itoa(m.Lost)},
		{FieldRecovered, itoa(m.Recovered)},
		{FieldRecoveryRate, formatRate(m.RecoveryRate)},
		{FieldP95MS, formatFloat(m.P95MS)},
		{FieldIndex, shortHash(m.Index)},
		{FieldOutputShaMatch, formatBool(m.OutputShaMatch)},
	})
}

// Header returns the column header matching Row.
func Header() string {
	return joinFields([]field{
		{FieldCase, ""}, {FieldSyncMaxMS, ""}, {FieldLost, ""},
		{FieldRecovered, ""}, {FieldRecoveryRate, ""}, {FieldP95MS, ""},
		{FieldIndex, ""}, {FieldOutputShaMatch, ""},
	})
}

// SortByCase orders records for deterministic output.
func SortByCase(ms []Metrics) {
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].Case < ms[j].Case })
}

func microsToMillis(us int64) float64 { return float64(us) / 1000.0 }
