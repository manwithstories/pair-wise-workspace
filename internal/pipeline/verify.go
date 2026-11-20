package pipeline

import (
	"encoding/json"
	"fmt"
	"os"

	"mediamap/internal/fault"
)

// Acceptance ranges for the demo metrics.
var metricRanges = []struct {
	name          string
	lo, hi        float64
	get           func(Summary) float64
}{
	{"av_sync_ms", 2.0, 20.0, func(s Summary) float64 { return s.AVSyncMs }},
	{"packet_recovery_pct", 70.0, 99.0, func(s Summary) float64 { return s.PacketRecoveryPct }},
	{"p95_latency_ms", 1.0, 500.0, func(s Summary) float64 { return s.P95LatencyMs }},
	{"alignment_error_ms", 0.0, 80.0, func(s Summary) float64 { return s.AlignmentErrorMs }},
}

// VerifyFile re-checks a written output file: the manifest hash must
// reproduce, segment references must be unique and ordered, fault offsets
// must be sane, and the summary metrics must sit inside the acceptance
// ranges. It returns the problems found; exit code 1 iff any exist.
func VerifyFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var out Output
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var problems []string
	if got := ManifestHash(out.Tracks); got != out.Summary.ManifestHash {
		problems = append(problems, fmt.Sprintf(
			"manifest hash mismatch: file says %s, recomputed %s",
			out.Summary.ManifestHash, got))
	}
	for _, tr := range out.Tracks {
		for _, rep := range tr.Reps {
			seen := map[string]bool{}
			prev := int64(-1)
			for _, seg := range rep.Segments {
				if seen[seg.URI] {
					problems = append(problems, fmt.Sprintf(
						"%s/%s: duplicate segment reference %q", tr.Name, rep.ID, seg.URI))
				}
				seen[seg.URI] = true
				if seg.Start < prev {
					problems = append(problems, fmt.Sprintf(
						"%s/%s: segment %q starts at %d before previous %d",
						tr.Name, rep.ID, seg.URI, seg.Start, prev))
				}
				prev = seg.Start
			}
		}
	}
	for _, f := range out.Faults {
		if f.Offset < fault.NoOffset {
			problems = append(problems, fmt.Sprintf(
				"%s[%s]: invalid source offset %d", f.Stage, f.Track, f.Offset))
		}
	}
	for _, m := range metricRanges {
		if v := m.get(out.Summary); v < m.lo || v > m.hi {
			problems = append(problems, fmt.Sprintf(
				"metric %s=%.2f outside [%.1f, %.1f]", m.name, v, m.lo, m.hi))
		}
	}
	return problems, nil
}
