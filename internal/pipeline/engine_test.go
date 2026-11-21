package pipeline

import (
	"path/filepath"
	"testing"

	"mediad/internal/verify"
)

func TestEngine_HealthyRunIsCompleteAndReproducible(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(t.TempDir(), "out")
	if _, err := Generate(root, true); err != nil {
		t.Fatal(err)
	}
	eng, err := NewEngine(root, out)
	if err != nil {
		t.Fatal(err)
	}
	res, err := eng.Run(0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Exit != verify.ExitOK {
		t.Fatalf("healthy run exit = %d, want 0", res.Exit)
	}
	// 6 studio + 6 field + 4 voice = 16 confirmed fragments.
	if res.Report.Segments != 16 {
		t.Fatalf("segments = %d, want 16", res.Report.Segments)
	}
	if len(res.Report.SilenceMS) == 0 {
		t.Fatal("expected silence intervals from the 400 ms windows")
	}
	// Studio and field fragments are byte-identical -> duplicate groups exist.
	if len(res.Report.Duplicates) == 0 {
		t.Fatal("expected cross-track duplicate groups")
	}
	if res.Report.FirstBadOffset != nil || res.Report.BlockedNode != "" {
		t.Fatalf("healthy run must have no defects: %+v", res.Report)
	}
	firstSHA := res.Report.SHA256

	// Rerun into the same out dir: checkpoints skip, digest must match.
	eng2, err := NewEngine(root, out)
	if err != nil {
		t.Fatal(err)
	}
	res2, err := eng2.Run(0)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Report.SHA256 != firstSHA {
		t.Fatalf("rerun sha256 drifted: %s vs %s", res2.Report.SHA256, firstSHA)
	}
}

func TestEngine_CorruptRunIsRecoverableWithOffsets(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(t.TempDir(), "out")
	l, err := Generate(root, false)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := NewEngine(root, out)
	if err != nil {
		t.Fatal(err)
	}
	res, err := eng.Run(0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Exit != verify.ExitRecoverable {
		t.Fatalf("corrupt run exit = %d, want 2", res.Exit)
	}
	if res.Report.FirstBadOffset == nil {
		t.Fatal("expected a first_bad_offset in the report")
	}
	// The reported offset must match one of the injected defects within 1 B.
	matched := false
	for _, c := range l.Corrupt {
		if d := res.Report.FirstBadOffset.Offset - c.ExpectOffset; d <= 1 && d >= -1 {
			matched = true
		}
	}
	if !matched {
		t.Fatalf("first_bad_offset %d matches no injected defect", res.Report.FirstBadOffset.Offset)
	}
	// The truncated fragment must not have merged into any duplicate group.
	for _, g := range res.Report.Duplicates {
		for _, s := range g.Sources {
			if (s.Track == "studio" && s.Index == 4) || (s.Track == "voiceover" && s.Index == 2) {
				t.Fatalf("corrupt fragment leaked into duplicate group: %+v", s)
			}
		}
	}
}

func TestEngine_InterruptThenResumeCompletes(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(t.TempDir(), "out")
	if _, err := Generate(root, true); err != nil {
		t.Fatal(err)
	}
	eng, err := NewEngine(root, out)
	if err != nil {
		t.Fatal(err)
	}
	interrupted, err := eng.Run(3)
	if err != nil {
		t.Fatal(err)
	}
	if interrupted.Exit != verify.ExitRecoverable || interrupted.Report.BlockedNode == "" {
		t.Fatalf("interrupted run must be recoverable with blocked node: %+v", interrupted.Report)
	}
	// Resume with a fresh engine over the same checkpoint directory.
	eng2, err := NewEngine(root, out)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := eng2.Run(0)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.Exit != verify.ExitOK || resumed.Report.Segments != 16 {
		t.Fatalf("resume must complete fully: exit=%d segments=%d", resumed.Exit, resumed.Report.Segments)
	}
}
