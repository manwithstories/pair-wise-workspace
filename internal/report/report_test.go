package report

import (
	"strings"
	"testing"

	"mediaflow/internal/transport"
)

func TestBuildRecoveryRate(t *testing.T) {
	in := Inputs{
		Case: "clean", SyncResidualUS: 5_000,
		Transport:    transport.Stats{LostMedia: 0, Recovered: 0},
		P95SegmentUS: 12_000, IndexHash: strings.Repeat("a", 64), OutputShaMatch: true,
	}
	m := Build(in)
	if m.RecoveryRate != 1.0 {
		t.Fatalf("no-loss recovery rate should be 1.0, got %v", m.RecoveryRate)
	}
	if m.SyncMaxMS != 5.0 || m.P95MS != 12.0 {
		t.Fatalf("unit conversion wrong: %+v", m)
	}

	in2 := Inputs{
		Case: "damage",
		Transport: transport.Stats{LostMedia: 100, Recovered: 99},
		IndexHash: "idx", OutputShaMatch: true,
	}
	m2 := Build(in2)
	if m2.RecoveryRate < 0.989 || m2.RecoveryRate > 0.991 {
		t.Fatalf("recovery rate wrong: %v", m2.RecoveryRate)
	}
}

func TestGates(t *testing.T) {
	th := DefaultThresholds()
	pass := Metrics{SyncMaxMS: 5, RecoveryRate: 1, P95MS: 10, OutputShaMatch: true}
	if r := pass.Evaluate(th, false); !r.Pass {
		t.Fatalf("should pass: %+v", r)
	}

	syncFail := pass
	syncFail.SyncMaxMS = 25
	if r := syncFail.Evaluate(th, false); r.Pass || !contains(r.Failures, FieldSyncMaxMS) {
		t.Fatalf("sync gate failed to trigger: %+v", r)
	}

	recoveryFail := pass
	recoveryFail.RecoveryRate = 0.90
	if r := recoveryFail.Evaluate(th, false); r.Pass {
		t.Fatal("recovery gate should fail")
	}
	// Damage case with allowed gaps is exempt from the recovery gate.
	if r := recoveryFail.Evaluate(th, true); !r.Pass {
		t.Fatalf("damage case should tolerate gaps: %+v", r)
	}

	shaFail := pass
	shaFail.OutputShaMatch = false
	if r := shaFail.Evaluate(th, true); r.Pass || !contains(r.Failures, FieldOutputShaMatch) {
		t.Fatal("sha gate should always apply")
	}
}

func TestRowContainsRequiredFields(t *testing.T) {
	m := Metrics{
		Case: "resume", SyncMaxMS: 1.234, Lost: 2, Recovered: 2,
		RecoveryRate: 1, P95MS: 3.5, Index: strings.Repeat("b", 64), OutputShaMatch: true,
	}
	row := m.Row()
	for _, f := range []string{
		FieldCase, FieldSyncMaxMS, FieldLost, FieldRecovered,
		FieldRecoveryRate, FieldP95MS, FieldIndex, FieldOutputShaMatch,
	} {
		if !strings.Contains(row, f+"=") {
			t.Fatalf("row missing field %s: %s", f, row)
		}
	}
	if !strings.HasPrefix(row, FieldCase+"=resume") {
		t.Fatalf("case must lead the row: %s", row)
	}
}

func TestSortByCase(t *testing.T) {
	ms := []Metrics{{Case: "resume"}, {Case: "clean"}, {Case: "damage"}}
	SortByCase(ms)
	if ms[0].Case != "clean" || ms[1].Case != "damage" || ms[2].Case != "resume" {
		t.Fatalf("sort wrong: %v", ms)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
