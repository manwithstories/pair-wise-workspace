package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
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
	fmt.Fprintf(&b, "trace_summary=%s\n", shortHash(r.TraceSummary))
	return b.String()
}

func shortHash(h string) string {
	if len(h) > 16 {
		return h[:16]
	}
	return h
}

// DigestInputs is a stable fingerprint of the run configuration, used to prove
// a resumed run consumed the same inputs as the reference run.
func DigestInputs(seed int64, segments int, durNS int64) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%d|%d|%d", seed, segments, durNS)))
	return hex.EncodeToString(h[:])
}

// Round3 rounds a fraction to three decimals without float formatting drift.
func Round3(v float64) float64 { return math.Round(v*1000) / 1000 }

// Acceptance thresholds, exported so the CLI and tests agree on one source.
const (
	MinRoundtripRate    = 0.99
	MinKeyframeHitRate  = 0.98
	MinSegmentHashMatch = 0.999999
)
