// Package report turns a pipeline result into the stable metric set the
// demo prints.  Field names are part of the interface: operators and
// acceptance scripts parse them, so they are not renamed between releases.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"mediacheck/internal/model"
	"mediacheck/internal/pipeline"
)

// Metrics is the machine-readable summary of one run.
type Metrics struct {
	SyncErrorMS      float64  `json:"sync_error_ms"`
	LossRecoveryPct  float64  `json:"loss_recovery_pct"`
	P95MS            float64  `json:"p95_ms"`
	QueuePeak        int      `json:"queue_peak"`
	DamagedRanges    []string `json:"damaged_ranges"`
	OutputSHA256     string   `json:"output_sha256"`
	OutputPath       string   `json:"output_path,omitempty"`
	Packets          int      `json:"packets"`
	Bytes            int64    `json:"bytes"`
	Faults           int      `json:"faults"`
	LostSlots        int      `json:"lost_slots"`
	RecoveredFEC     int      `json:"recovered_fec"`
	RecoveredPLC     int      `json:"recovered_plc"`
	RecoveredSilence int      `json:"recovered_silence"`
	Unrecovered      int      `json:"unrecovered"`
	KeyframeInterval float64  `json:"keyframe_interval_ms"`
	Keyframes        int      `json:"keyframes"`
	FaultKinds       []string `json:"fault_kinds"`
	Canceled         bool     `json:"canceled"`
	ElapsedMS        float64  `json:"elapsed_ms"`
	Targets          Targets  `json:"targets"`
}

// Targets records whether each acceptance threshold was met.
type Targets struct {
	SyncErrorWithin20MS   bool `json:"sync_error_within_20ms"`
	LossRecoveryAtLeast95 bool `json:"loss_recovery_at_least_95pct"`
	P95Within250MS        bool `json:"p95_within_250ms"`
	QueueWithinBound      bool `json:"queue_within_bound"`
}

// Build derives Metrics from a run result and the delivered file hash.
func Build(res *pipeline.Result, outputPath, outputSHA string) Metrics {
	m := Metrics{
		SyncErrorMS:      res.SyncErrorMillis(),
		LossRecoveryPct:  res.Recovery.RecoveryRate(),
		P95MS:            res.P95Millis(),
		QueuePeak:        res.Queue.PeakItems,
		OutputSHA256:     outputSHA,
		OutputPath:       outputPath,
		Packets:          res.Packets,
		Bytes:            res.Bytes,
		Faults:           len(res.Faults),
		LostSlots:        res.Recovery.Slots,
		RecoveredFEC:     res.Recovery.FEC,
		RecoveredPLC:     res.Recovery.PLC,
		RecoveredSilence: res.Recovery.Silence,
		Unrecovered:      res.Recovery.Failed,
		Canceled:         res.Canceled,
		ElapsedMS:        float64(res.Elapsed) / float64(time.Millisecond),
	}
	for _, r := range res.Damage.Ranges() {
		m.DamagedRanges = append(m.DamagedRanges, r.String())
	}
	// Fault kinds are deduplicated and sorted so the summary is stable
	// regardless of the order the stages observed them in.
	seen := map[string]struct{}{}
	for _, f := range res.Faults {
		name := f.Kind.String()
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		m.FaultKinds = append(m.FaultKinds, name)
	}
	sort.Strings(m.FaultKinds)
	if v, ok := res.Inspection.Streams[model.TrackVideo]; ok {
		m.KeyframeInterval = v.KeyframeInterval()
		m.Keyframes = v.Keyframes
	}
	m.Targets = Targets{
		SyncErrorWithin20MS:   m.SyncErrorMS <= 20,
		LossRecoveryAtLeast95: m.LossRecoveryPct >= 95,
		P95Within250MS:        m.P95MS <= 250,
		QueueWithinBound:      res.Queue.PeakItems <= res.Queue.MaxItems,
	}
	return m
}

// AllTargetsMet reports whether every acceptance threshold passed.  A clean
// run with no lost slots trivially passes the recovery target.
func (m Metrics) AllTargetsMet() bool {
	t := m.Targets
	return t.SyncErrorWithin20MS && t.LossRecoveryAtLeast95 && t.P95Within250MS && t.QueueWithinBound
}

// ExitCode maps a run to the documented process exit codes: 0 for a clean
// deliverable, 2 when damage was found and handled, 1 for internal failure,
// 130 for cancellation.
func ExitCode(m Metrics) int {
	if m.Canceled {
		return 130
	}
	if m.Faults > 0 || len(m.DamagedRanges) > 0 {
		return 2
	}
	return 0
}

// Print writes the demo output.  The first block is the stable
// key=value contract; the second is human context.
func Print(w io.Writer, m Metrics, extra []string) {
	fmt.Fprintf(w, "sync_error_ms=%.3f\n", m.SyncErrorMS)
	fmt.Fprintf(w, "loss_recovery_pct=%.2f\n", m.LossRecoveryPct)
	fmt.Fprintf(w, "p95_ms=%.3f\n", m.P95MS)
	fmt.Fprintf(w, "queue_peak=%d\n", m.QueuePeak)
	fmt.Fprintf(w, "damaged_ranges=%s\n", strings.Join(m.DamagedRanges, ","))
	if m.OutputSHA256 != "" {
		fmt.Fprintf(w, "output_sha256=%s\n", m.OutputSHA256)
	} else {
		fmt.Fprintf(w, "output_sha256=none\n")
	}
	fmt.Fprintf(w, "packets=%d\n", m.Packets)
	fmt.Fprintf(w, "bytes=%d\n", m.Bytes)
	fmt.Fprintf(w, "faults=%d\n", m.Faults)
	fmt.Fprintf(w, "lost_slots=%d\n", m.LostSlots)
	fmt.Fprintf(w, "recovered_fec=%d\n", m.RecoveredFEC)
	fmt.Fprintf(w, "recovered_plc=%d\n", m.RecoveredPLC)
	fmt.Fprintf(w, "recovered_silence=%d\n", m.RecoveredSilence)
	fmt.Fprintf(w, "unrecovered=%d\n", m.Unrecovered)
	fmt.Fprintf(w, "keyframe_interval_ms=%.3f\n", m.KeyframeInterval)
	fmt.Fprintf(w, "keyframes=%d\n", m.Keyframes)
	fmt.Fprintf(w, "fault_kinds=%s\n", strings.Join(m.FaultKinds, ","))
	fmt.Fprintf(w, "elapsed_ms=%.3f\n", m.ElapsedMS)
	fmt.Fprintf(w, "targets_met=%t\n", m.AllTargetsMet())
	fmt.Fprintf(w, "exit_code=%d\n", ExitCode(m))
	for _, line := range extra {
		fmt.Fprintln(w, line)
	}
}

// Summary renders the damaged ranges sorted and capped for a one-line log,
// so a terminal operator sees the worst areas without scrolling a thousand
// entries.
func Summary(ranges []string, limit int) string {
	if len(ranges) == 0 {
		return "none"
	}
	sorted := append([]string(nil), ranges...)
	sort.Strings(sorted)
	if limit > 0 && len(sorted) > limit {
		return fmt.Sprintf("%s (+%d more)", strings.Join(sorted[:limit], ","), len(sorted)-limit)
	}
	return strings.Join(sorted, ",")
}

// JSONLines renders the metrics as a single compact JSON object.  encoding/json
// is used rather than hand formatting so the keys stay in sync with the struct
// tags the acceptance scripts rely on.
func JSONLines(m Metrics) []string {
	b, err := json.Marshal(m)
	if err != nil {
		return []string{`{"error":"could not marshal metrics"}`}
	}
	return []string{string(b)}
}
