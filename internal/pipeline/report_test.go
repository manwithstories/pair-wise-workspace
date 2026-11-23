package pipeline

import (
	"strings"
	"testing"
)

func TestParseFaults(t *testing.T) {
	f, err := ParseFaults("reorder,truncate,drift")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !f.Reorder || !f.Truncate || !f.Drift || f.Drop || f.Wrap {
		t.Fatalf("parsed = %+v", f)
	}
	if _, err := ParseFaults("bogus"); err == nil {
		t.Fatalf("unknown fault accepted")
	}
	if _, err := ParseFaults(""); err != nil {
		t.Fatalf("empty fault list rejected: %v", err)
	}
}

func TestFormatRatesContainsRequiredFields(t *testing.T) {
	res := Result{
		RoundtripRate: 1, KeyframeHitRate: 1, SegmentHashMatch: 1,
		RecoveredGaps: 12, FinalGaps: 0, RollbackCount: 2,
		ResumeHashEqual: true, ProvenanceComplete: true,
		TraceSummary: "abc123def4567890",
	}
	out := res.FormatRates()
	for _, field := range []string{
		"roundtrip_rate=", "keyframe_hit_rate=", "segment_hash_match=",
		"recovered_gaps=", "rollback_count=", "resume_hash_equal=",
		"provenance_complete=",
	} {
		if !strings.Contains(out, field) {
			t.Fatalf("missing %q in:\n%s", field, out)
		}
	}
}

func TestAcceptanceThresholds(t *testing.T) {
	cases := []struct {
		rt, hit, hash float64
		want          bool
	}{
		{1, 1, 1, true},
		{0.99, 0.98, 1, true},
		{0.9899, 1, 1, false},
		{1, 0.9799, 1, false},
		{1, 1, 0.999, false},
	}
	for _, c := range cases {
		inv := Invariants{Roundtrip: c.rt, KeyframeHit: c.hit,
			SegmentHashes: c.hash, ResumeEqual: true, ResumeChecked: true,
			Provenance: true}
		if inv.Pass() != c.want {
			t.Fatalf("%+v: pass = %v", c, inv.Pass())
		}
	}
}

func TestInvariantsRequireResumeAndProvenance(t *testing.T) {
	inv := Invariants{Roundtrip: 1, KeyframeHit: 1, SegmentHashes: 1,
		ResumeEqual: false, ResumeChecked: true, Provenance: true}
	if inv.Pass() {
		t.Fatalf("false resume equality accepted")
	}
	inv.ResumeEqual = true
	inv.ResumeChecked = false
	if inv.Pass() {
		t.Fatalf("unverified resume accepted")
	}
	inv.ResumeChecked = true
	inv.Provenance = false
	if inv.Pass() {
		t.Fatalf("incomplete provenance accepted")
	}
}
