// Package report aggregates relay engine and signaling counters.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"

	"pairwise.local/relayctl/internal/relay"
	"pairwise.local/relayctl/internal/signal"
)

// Jitter is the compact aggregate shown in the stdout contract.
type Jitter struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	Max float64 `json:"max"`
}

// Egress is one independently measured outlet.
type Egress struct {
	Endpoint  string  `json:"endpoint"`
	SSRC      uint32  `json:"ssrc"`
	Forwarded uint64  `json:"forwarded"`
	Dropped   uint64  `json:"dropped"`
	Late      uint64  `json:"late"`
	JitterP50 float64 `json:"jitter_p50_ms"`
	JitterP95 float64 `json:"jitter_p95_ms"`
	JitterMax float64 `json:"jitter_max_ms"`
	Samples   int     `json:"jitter_samples"`
	Failed    bool    `json:"failed"`
}

// Report is persisted and emitted as one JSON line.
type Report struct {
	CandidatePairs   int      `json:"candidate_pairs"`
	ForwardedPackets uint64   `json:"forwarded_packets"`
	DroppedPackets   uint64   `json:"dropped_packets"`
	LatePackets      uint64   `json:"late_packets"`
	JitterMS         Jitter   `json:"jitter_ms"`
	EgressFailed     int      `json:"egress_failed"`
	IngressPackets   uint64   `json:"ingress_packets"`
	SignalRejected   uint64   `json:"signal_rejected"`
	SignalTimeouts   uint64   `json:"signal_timeouts"`
	Egresses         []Egress `json:"egresses"`
}

// Build converts engine and signaling snapshots into a stable report.
func Build(engine relay.Stats, signaling signal.Stats) Report {
	result := Report{
		CandidatePairs:   engine.CandidatePairs,
		ForwardedPackets: engine.ForwardedPackets,
		DroppedPackets:   engine.DroppedPackets,
		LatePackets:      engine.LatePackets,
		EgressFailed:     engine.EgressFailed,
		IngressPackets:   engine.IngressPackets,
		SignalRejected:   signaling.Rejected,
		SignalTimeouts:   signaling.HandshakeTimeouts,
		JitterMS: Jitter{
			P50: engine.JitterMS.P50,
			P95: engine.JitterMS.P95,
			Max: engine.JitterMS.Max,
		},
		Egresses: make([]Egress, 0, len(engine.Egresses)),
	}
	for _, egress := range engine.Egresses {
		result.Egresses = append(result.Egresses, Egress{
			Endpoint: egress.Endpoint, SSRC: egress.SSRC,
			Forwarded: egress.Forwarded, Dropped: egress.Dropped, Late: egress.Late,
			JitterP50: egress.JitterMS.P50, JitterP95: egress.JitterMS.P95,
			JitterMax: egress.JitterMS.Max, Samples: egress.JitterMS.Samples,
			Failed: egress.Failed,
		})
	}
	return result
}

// WriteFile writes indented JSON so the artifact is human-readable.
func WriteFile(path string, result Report) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// WriteLine emits the single-line JSON stdout contract.
func WriteLine(writer io.Writer, result Report) error {
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(writer, "%s\n", data)
	return err
}

// Load reads a report produced by WriteFile.
func Load(path string) (Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Report{}, err
	}
	var result Report
	if err := json.Unmarshal(data, &result); err != nil {
		return Report{}, err
	}
	return result, nil
}

// WriteTable renders a compact terminal summary.
func WriteTable(writer io.Writer, result Report) error {
	if _, err := fmt.Fprintln(writer, "RELAY VERIFY REPORT"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writer,
		"candidate_pairs=%d forwarded=%d dropped=%d late=%d failed=%d\n",
		result.CandidatePairs, result.ForwardedPackets, result.DroppedPackets,
		result.LatePackets, result.EgressFailed); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writer, "jitter_ms p50=%.3f p95=%.3f max=%.3f\n\n",
		result.JitterMS.P50, result.JitterMS.P95, result.JitterMS.Max); err != nil {
		return err
	}
	table := tabwriter.NewWriter(writer, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "ENDPOINT\tSSRC\tFORWARDED\tDROPPED\tLATE\tP95_MS\tSAMPLES\tFAILED"); err != nil {
		return err
	}
	for _, egress := range result.Egresses {
		if _, err := fmt.Fprintf(table, "%s\t%d\t%d\t%d\t%d\t%.3f\t%d\t%t\n",
			egress.Endpoint, egress.SSRC, egress.Forwarded, egress.Dropped,
			egress.Late, egress.JitterP95, egress.Samples, egress.Failed); err != nil {
			return err
		}
	}
	return table.Flush()
}
