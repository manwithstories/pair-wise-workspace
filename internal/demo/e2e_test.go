package demo_test

import (
	"testing"

	"archivectl/internal/audit"
	"archivectl/internal/demo"
	"archivectl/internal/pipeline"
)

// TestEndToEndCleanAndDamaged exercises generation, crash/resume and the
// independent auditor for both profiles. It mirrors the acceptance run.
func TestEndToEndCleanAndDamaged(t *testing.T) {
	root := t.TempDir()
	const segments = 16
	if err := demo.Generate(root, segments, 7, demo.ProfileBoundaryV1); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.Run(root, "clean", pipeline.Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.ResumeCheck(root, "damaged", segments); err != nil {
		t.Fatal(err)
	}

	for _, group := range []string{"clean", "damaged"} {
		m, err := audit.Verify(root, group, 2)
		if err != nil {
			t.Fatalf("%s verify: %v (reasons=%v)", group, err, m.FailReasons)
		}
		if !m.Pass || !m.RepeatMatch || !m.DigestInvalidated || !m.InputDigestMatch || !m.PayloadMatch {
			t.Fatalf("%s invariants false: %+v", group, m)
		}
		if m.AVSyncMaxMS > 1.0 || m.LossRecovery < 0.90 || m.SegmentP95MS > 40 {
			t.Fatalf("%s metrics over threshold: %+v", group, m)
		}
		if len(m.IndexSHA256) != 64 {
			t.Fatalf("%s index digest bad: %q", group, m.IndexSHA256)
		}
		if m.SeekChecks == 0 {
			t.Fatalf("%s no seek checks performed", group)
		}
	}

	damaged, _ := audit.Verify(root, "damaged", 2)
	if damaged.UnrecoveredGaps != 1 || damaged.UnrecoveredPackets != 2 {
		t.Fatalf("damaged gaps=%d packets=%d want 1/2", damaged.UnrecoveredGaps, damaged.UnrecoveredPackets)
	}
	if damaged.StructuralFaults != 1 || damaged.MetadataConflicts != 1 {
		t.Fatalf("damaged fixtures not detected: structural=%d meta=%d",
			damaged.StructuralFaults, damaged.MetadataConflicts)
	}
	clean, _ := audit.Verify(root, "clean", 2)
	if clean.UnrecoveredGaps != 0 || clean.MetadataConflicts != 0 {
		t.Fatalf("clean profile should have no gaps/conflicts: %+v", clean)
	}
}

// TestUnknownProfileRejected ensures argument validation surfaces early.
func TestUnknownProfileRejected(t *testing.T) {
	if err := demo.Generate(t.TempDir(), 8, 1, "nope"); err == nil {
		t.Fatal("unknown fault profile must be rejected")
	}
}
