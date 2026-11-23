package pipeline

import (
	"fmt"
	"strings"
)

// ParseFaults maps CLI fault words to the Faults struct.
func ParseFaults(s string) (Faults, error) {
	var f Faults
	for _, part := range strings.Split(s, ",") {
		switch strings.ToLower(strings.TrimSpace(part)) {
		case "", "none":
			continue
		case "reorder":
			f.Reorder = true
		case "truncate":
			f.Truncate = true
		case "drift":
			f.Drift = true
		case "drop":
			f.Drop = true
		case "wrap":
			f.Wrap = true
		default:
			return f, fmt.Errorf("unknown fault %q", part)
		}
	}
	return f, nil
}

// FormatRates renders the terminal summary lines the acceptance check reads.
// Rates are printed to four decimals; the thresholds are asserted on the
// underlying values, not on the rounded text.
func (r Result) FormatRates() string {
	var b strings.Builder
	fmt.Fprintf(&b, "roundtrip_rate=%.4f\n", r.RoundtripRate)
	fmt.Fprintf(&b, "keyframe_hit_rate=%.4f\n", r.KeyframeHitRate)
	fmt.Fprintf(&b, "segment_hash_match=%.3f\n", r.SegmentHashMatch)
	fmt.Fprintf(&b, "recovered_gaps=%d\n", r.RecoveredGaps)
	fmt.Fprintf(&b, "final_gaps=%d\n", r.FinalGaps)
	fmt.Fprintf(&b, "rollback_count=%d\n", r.RollbackCount)
	fmt.Fprintf(&b, "resume_hash_equal=%t\n", r.ResumeHashEqual)
	fmt.Fprintf(&b, "provenance_complete=%t\n", r.ProvenanceComplete)
	fmt.Fprintf(&b, "keyframes=%d\n", r.Keyframes)
	fmt.Fprintf(&b, "committed_intervals=%d\n", r.CommittedIntervals)
	fmt.Fprintf(&b, "segments=%d\n", r.Segments)
	if r.DriftSlopePPB != 0 || r.DriftAmbiguous {
		fmt.Fprintf(&b, "drift_slope_ppb=%d\n", r.DriftSlopePPB)
		if r.DriftAmbiguous {
			fmt.Fprintf(&b, "drift_fit=ambiguous\n")
		}
	}
	fmt.Fprintf(&b, "trace_summary=%s\n", shortHash(r.TraceSummary))
	return b.String()
}

func shortHash(h string) string {
	if len(h) > 16 {
		return h[:16]
	}
	return h
}

// Acceptance thresholds, exported so the CLI and tests agree on one source.
const (
	MinRoundtripRate    = 0.99
	MinKeyframeHitRate  = 0.98
	MinSegmentHashMatch = 0.999999
)

// Invariants collects the acceptance thresholds and the boolean guarantees
// the demo must not soften. ResumeChecked distinguishes "resume equality was
// verified" from "resume equality is trivially true because no resume ran".
type Invariants struct {
	Roundtrip     float64
	KeyframeHit   float64
	SegmentHashes float64
	ResumeEqual   bool
	ResumeChecked bool
	Provenance    bool
}

// Pass reports whether every documented invariant holds. The full acceptance
// contract requires a verified resume, so an unchecked resume cannot pass.
func (v Invariants) Pass() bool {
	return v.Roundtrip >= MinRoundtripRate &&
		v.KeyframeHit >= MinKeyframeHitRate &&
		v.SegmentHashes >= MinSegmentHashMatch &&
		v.ResumeChecked && v.ResumeEqual &&
		v.Provenance
}
