package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"relaycheck/internal/fixture"
)

func genDirs(t *testing.T) (clean, edge string) {
	t.Helper()
	root := t.TempDir()
	clean = filepath.Join(root, "clean")
	edge = filepath.Join(root, "edge")
	spec := fixture.DefaultSpec(17)
	if err := fixture.Generate(clean, spec, false); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Generate(edge, spec, true); err != nil {
		t.Fatal(err)
	}
	return clean, edge
}

func TestCleanRunCompletes(t *testing.T) {
	clean, _ := genDirs(t)
	idx := filepath.Join(t.TempDir(), "idx")
	res, err := Run(context.Background(), Config{
		InputDir: clean, IndexPath: idx, Chunk: 4, Queue: 2, MemBytes: 1 << 24,
	})
	if err != nil || res.ExitCode != ExitOK {
		t.Fatalf("err=%v exit=%d", err, res.ExitCode)
	}
	if res.Summary.Segments != 12 || res.Summary.Samples != 48 {
		t.Fatalf("segments=%d samples=%d", res.Summary.Segments, res.Summary.Samples)
	}
	if res.Summary.FaultCount != 0 || res.Summary.FirstBad != -1 {
		t.Fatalf("clean run should have no faults: %+v", res.Summary)
	}
	if res.Summary.MemoryPeakBytes > 1<<24 {
		t.Fatalf("peak %d exceeds budget", res.Summary.MemoryPeakBytes)
	}
}

func TestPauseExit75AndResumeIdempotent(t *testing.T) {
	clean, _ := genDirs(t)
	root := t.TempDir()
	idx := filepath.Join(root, "idx")
	cfg := Config{
		InputDir: clean, IndexPath: idx, Chunk: 4, Queue: 2,
		MemBytes: 1 << 24, PauseAfter: 3,
	}
	paused, err := Run(context.Background(), cfg)
	if err != nil || paused.ExitCode != ExitPaused {
		t.Fatalf("pause err=%v exit=%d", err, paused.ExitCode)
	}
	if _, statErr := os.Stat(filepath.Join(idx, "checkpoint.0.json")); statErr != nil {
		t.Fatalf("checkpoint not written: %v", statErr)
	}
	cfg.Resume = true
	resumed, err := Run(context.Background(), cfg)
	if err != nil || resumed.ExitCode != ExitOK {
		t.Fatalf("resume err=%v exit=%d", err, resumed.ExitCode)
	}
	if resumed.Summary.ResumeMS < 1 {
		t.Fatalf("resume_ms = %d", resumed.Summary.ResumeMS)
	}
	// 完整运行的 SHA 必须与暂停+续跑一致（幂等）。
	fullIdx := filepath.Join(root, "full")
	full, err := Run(context.Background(), Config{
		InputDir: clean, IndexPath: fullIdx, Chunk: 4, Queue: 2, MemBytes: 1 << 24,
	})
	if err != nil {
		t.Fatal(err)
	}
	if full.Summary.SHA256 != resumed.Summary.SHA256 {
		t.Fatalf("sha mismatch full=%s resumed=%s", full.Summary.SHA256, resumed.Summary.SHA256)
	}
}

func TestEdgeRunFindsAllFaultClasses(t *testing.T) {
	_, edge := genDirs(t)
	res, err := Run(context.Background(), Config{
		InputDir: edge, IndexPath: filepath.Join(t.TempDir(), "idx"),
		Chunk: 4, Queue: 2, MemBytes: 1 << 24,
	})
	if err != nil || res.ExitCode != ExitOK {
		t.Fatalf("err=%v exit=%d", err, res.ExitCode)
	}
	kinds := map[string]bool{}
	for _, f := range res.Summary.Faults {
		kinds[f.Kind] = true
	}
	for _, want := range []string{
		"manifest_missing_segment", "mp4_box_truncated",
		"rtp_reordered", "rtp_sequence_wrap", "rtp_packet_loss",
		"rtp_timestamp_jump", "fragment_oversized",
	} {
		if !kinds[want] {
			t.Fatalf("missing fault class %s in %+v", want, kinds)
		}
	}
	if res.Summary.FirstBad < 0 {
		t.Fatal("edge run should report a first bad offset")
	}
	if res.Summary.OffsetDeltaBytes > 16 {
		t.Fatalf("offset delta %d exceeds 16 bytes", res.Summary.OffsetDeltaBytes)
	}
}

func TestRejectsChangedInput(t *testing.T) {
	clean, _ := genDirs(t)
	root := t.TempDir()
	idx := filepath.Join(root, "idx")
	cfg := Config{InputDir: clean, IndexPath: idx, Chunk: 4, Queue: 2, MemBytes: 1 << 24}
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	// 篡改输入文件后续跑必须被拒绝。
	target := filepath.Join(clean, "seg0.m4s")
	original, _ := os.ReadFile(target)
	tampered := append(append([]byte{}, original...), 0x01)
	if err := os.WriteFile(target, tampered, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.Resume = true
	res, err := Run(context.Background(), cfg)
	if err == nil || res.ExitCode != ExitBadInput {
		t.Fatalf("expected rejection, err=%v exit=%d", err, res.ExitCode)
	}
}

func TestInvalidConfigExit2(t *testing.T) {
	cases := []Config{
		{InputDir: "", IndexPath: "x", Chunk: 4, Queue: 2, MemBytes: 1},
		{InputDir: "x", IndexPath: "y", Chunk: 0, Queue: 2, MemBytes: 1},
		{InputDir: "x", IndexPath: "y", Chunk: 4, Queue: 0, MemBytes: 1},
		{InputDir: "x", IndexPath: "y", Chunk: 4, Queue: 2, MemBytes: 0},
	}
	for i, c := range cases {
		if _, err := Run(context.Background(), c); err == nil {
			t.Fatalf("case %d should error", i)
		}
	}
}
