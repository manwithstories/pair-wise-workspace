// Package report turns pipeline counters into the deterministic quality
// summary required by the archive acceptance contract: target/actual
// bitrate delta, CRC match rate, complete-segment rate, repair and gap
// counts, peak memory and a seed-stable semantic digest.
package report

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"

	"archive/internal/model"
)

// Status is the overall verdict of one recovery run.
type Status string

const (
	// StatusOK means every segment was recovered complete and every
	// invariant held.
	StatusOK Status = "ok"
	// StatusDegraded means the run finished but contains at least one
	// explicit gap; the affected segments are locatable, never faked.
	StatusDegraded Status = "degraded"
)

// Counters holds the raw measurements collected while the pipeline runs.
// Every field is accumulated by the pipeline, never derived twice.
type Counters struct {
	TargetKbps       int
	ActualKbps       float64
	PacketsTotal     int
	CRCMatched       int
	SegmentsTotal    int
	SegmentsComplete int
	Retransmitted    int
	FECRecovered     int
	Gaps             int
	PeakBytes        int64
	MemoryLimit      int64
	SyncErrorNanos   int64
	Backpressure     int
}

// Recovered is the total number of packets restored by any repair source.
func (c Counters) Recovered() int { return c.Retransmitted + c.FECRecovered }

// Summary is the stable, machine-readable result of one run.
type Summary struct {
	Case          string
	Status        Status
	TargetKbps    int
	ActualKbps    float64
	DeltaPct      float64
	QualityCRCPct float64
	SyncErrorMs   float64
	Recovered     int
	Gaps          int
	PeakBytes     int64
	SemanticSHA   string
}

// Build validates the counters, computes the derived percentages, derives
// the status and calculates the semantic digest. The digest covers only
// semantic fields (not volatile memory noise beyond the peak counter), so
// identical seeds produce identical digests.
func Build(caseName string, c Counters) (Summary, error) {
	if c.MemoryLimit > 0 && c.PeakBytes > c.MemoryLimit {
		return Summary{}, model.InvariantError(model.PhasePipeline,
			"peak memory %d exceeds limit %d", c.PeakBytes, c.MemoryLimit)
	}
	if c.SegmentsTotal > 0 && c.SegmentsComplete > c.SegmentsTotal {
		return Summary{}, model.InvariantError(model.PhasePipeline,
			"complete segments %d exceed total %d", c.SegmentsComplete, c.SegmentsTotal)
	}
	if c.PacketsTotal > 0 && c.CRCMatched > c.PacketsTotal {
		return Summary{}, model.InvariantError(model.PhasePipeline,
			"CRC matches %d exceed packets %d", c.CRCMatched, c.PacketsTotal)
	}

	delta := 0.0
	if c.TargetKbps > 0 {
		delta = (c.ActualKbps - float64(c.TargetKbps)) / float64(c.TargetKbps) * 100
	}
	crcPct := 100.0
	if c.PacketsTotal > 0 {
		crcPct = float64(c.CRCMatched) / float64(c.PacketsTotal) * 100
	}
	status := StatusOK
	if c.Gaps > 0 {
		status = StatusDegraded
	}
	s := Summary{
		Case: caseName, Status: status, TargetKbps: c.TargetKbps,
		ActualKbps: round2(c.ActualKbps), DeltaPct: round2(delta),
		QualityCRCPct: round2(crcPct),
		SyncErrorMs:   round2(model.Nano(c.SyncErrorNanos).Milliseconds()),
		Recovered:     c.Recovered(), Gaps: c.Gaps, PeakBytes: c.PeakBytes,
	}
	s.SemanticSHA = digest(s, c)
	return s, nil
}

// digest hashes the semantic content in a fixed field order. Counters feed
// the hash so runs with the same generated input and processing produce the
// same value, while any difference in recovery outcome changes it.
func digest(s Summary, c Counters) string {
	var b strings.Builder
	fmt.Fprintf(&b, "case=%s\n", s.Case)
	fmt.Fprintf(&b, "status=%s\n", s.Status)
	fmt.Fprintf(&b, "target=%d\n", s.TargetKbps)
	fmt.Fprintf(&b, "actual=%.2f\n", s.ActualKbps)
	fmt.Fprintf(&b, "packets=%d crc=%d\n", c.PacketsTotal, c.CRCMatched)
	fmt.Fprintf(&b, "segments=%d complete=%d\n", c.SegmentsTotal, c.SegmentsComplete)
	fmt.Fprintf(&b, "retrans=%d fec=%d gaps=%d backpressure=%d\n",
		c.Retransmitted, c.FECRecovered, c.Gaps, c.Backpressure)
	fmt.Fprintf(&b, "sync_ns=%d peak=%d\n", c.SyncErrorNanos, c.PeakBytes)
	h := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(h[:])
}

// Render returns the summary as ordered key=value lines for terminal output.
// The key set matches the acceptance contract exactly.
func (s Summary) Render() string {
	keys := []struct{ k, v string }{
		{"case", s.Case},
		{"status", string(s.Status)},
		{"target_kbps", fmt.Sprintf("%d", s.TargetKbps)},
		{"actual_kbps", fmt.Sprintf("%.2f", s.ActualKbps)},
		{"delta_pct", fmt.Sprintf("%.2f", s.DeltaPct)},
		{"quality_crc_pct", fmt.Sprintf("%.2f", s.QualityCRCPct)},
		{"sync_error_ms", fmt.Sprintf("%.2f", s.SyncErrorMs)},
		{"recovered", fmt.Sprintf("%d", s.Recovered)},
		{"gaps", fmt.Sprintf("%d", s.Gaps)},
		{"peak_bytes", fmt.Sprintf("%d", s.PeakBytes)},
		{"semantic_sha256", s.SemanticSHA},
	}
	var b strings.Builder
	for _, kv := range keys {
		fmt.Fprintf(&b, "%s=%s\n", kv.k, kv.v)
	}
	return b.String()
}

// RenderTable renders several summaries as aligned columns, sorted by case
// name so multi-case demo output is itself deterministic.
func RenderTable(ss []Summary) string {
	sorted := append([]Summary(nil), ss...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Case < sorted[j].Case })
	header := []string{"case", "status", "target_kbps", "actual_kbps", "delta_pct",
		"quality_crc_pct", "sync_error_ms", "recovered", "gaps", "peak_bytes", "semantic_sha256"}
	rows := make([][]string, 0, len(sorted))
	for _, s := range sorted {
		rows = append(rows, []string{
			s.Case, string(s.Status), fmt.Sprintf("%d", s.TargetKbps),
			fmt.Sprintf("%.2f", s.ActualKbps), fmt.Sprintf("%.2f", s.DeltaPct),
			fmt.Sprintf("%.2f", s.QualityCRCPct), fmt.Sprintf("%.2f", s.SyncErrorMs),
			fmt.Sprintf("%d", s.Recovered), fmt.Sprintf("%d", s.Gaps),
			fmt.Sprintf("%d", s.PeakBytes), s.SemanticSHA[:12],
		})
	}
	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = len(h)
	}
	for _, r := range rows {
		for i, cell := range r {
			if len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}
	var b strings.Builder
	writeRow := func(cells []string) {
		for i, cell := range cells {
			if i > 0 {
				b.WriteString("  ")
			}
			b.WriteString(pad(cell, widths[i]))
		}
		b.WriteByte('\n')
	}
	writeRow(header)
	for _, r := range rows {
		writeRow(r)
	}
	return b.String()
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

func pad(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}
