package export

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"medpipe/internal/fault"
	rtpstream "medpipe/internal/rtp"
)

func testParams(dir string) RunParams {
	return RunParams{
		Seed:           41,
		Tracks:         3,
		Packets:        12000,
		Loss:           0.02,
		ReorderWindow:  64,
		Workers:        4,
		Queue:          256,
		CheckpointPath: filepath.Join(dir, "checkpoint.json"),
		OutDir:         filepath.Join(dir, "out"),
	}
}

func runOnce(t *testing.T, params RunParams) *Metrics {
	t.Helper()
	m, err := Run(context.Background(), params)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := Verify(context.Background(), params.OutDir); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	return m
}

func TestEndToEndDeterministic(t *testing.T) {
	m1 := runOnce(t, testParams(t.TempDir()))
	m2 := runOnce(t, testParams(t.TempDir()))
	if m1.OutputSHA256 != m2.OutputSHA256 {
		t.Fatalf("same input produced different outputs:\n%s\n%s",
			m1.OutputSHA256, m2.OutputSHA256)
	}
	if m1.FramesWritten < 1000 {
		t.Fatalf("frames_written = %d, want thousands", m1.FramesWritten)
	}
	if m1.PacketsTotal != 12000 || m1.ProvenanceErrors != 0 {
		t.Fatalf("metrics = %+v", m1)
	}
	if m1.PacketsLost != m1.PacketsTotal-m1.FramesWritten {
		t.Fatalf("loss accounting inconsistent: %+v", m1)
	}
}

func TestResumeProducesIdenticalOutput(t *testing.T) {
	dir := t.TempDir()
	params := testParams(dir)
	full := runOnce(t, params)

	params.Resume = true
	m, err := Run(context.Background(), params)
	if err != nil {
		t.Fatalf("resume Run: %v", err)
	}
	if m.OutputSHA256 != full.OutputSHA256 {
		t.Fatalf("resume changed output:\n%s\n%s", m.OutputSHA256, full.OutputSHA256)
	}
	if m.FramesWritten != full.FramesWritten || m.PacketsLost != full.PacketsLost {
		t.Fatalf("resume metrics differ: %+v vs %+v", m, full)
	}
	if m.ResumeSeconds >= full.ResumeSeconds {
		t.Fatalf("resume not faster: %.3f vs %.3f", m.ResumeSeconds, full.ResumeSeconds)
	}
}

func TestPartialResumeAfterSegmentCorruption(t *testing.T) {
	dir := t.TempDir()
	params := testParams(dir)
	full := runOnce(t, params)

	// Simulate an interrupted run: checkpoint not complete, one
	// segment damaged on disk.
	cp, err := LoadCheckpoint(params.CheckpointPath)
	if err != nil || cp == nil {
		t.Fatalf("load checkpoint: %v", err)
	}
	cp.Complete = false
	if err := SaveCheckpoint(params.CheckpointPath, cp); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(params.OutDir, segBinRel(1))
	data, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	data[100] ^= 0xFF
	if err := os.WriteFile(victim, data, 0o644); err != nil {
		t.Fatal(err)
	}

	params.Resume = true
	m, err := Run(context.Background(), params)
	if err != nil {
		t.Fatalf("partial resume: %v", err)
	}
	if m.OutputSHA256 != full.OutputSHA256 {
		t.Fatalf("partial resume changed output")
	}
	if _, err := Verify(context.Background(), params.OutDir); err != nil {
		t.Fatalf("Verify after partial resume: %v", err)
	}
}

func TestCorruptCheckpointFallsBack(t *testing.T) {
	dir := t.TempDir()
	params := testParams(dir)
	if err := os.MkdirAll(filepath.Dir(params.CheckpointPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(params.CheckpointPath, []byte("{garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	params.Resume = true
	m := runOnce(t, params)
	if m.FramesWritten == 0 {
		t.Fatal("fallback run produced no frames")
	}
}

func TestClockJumpIsolationAppearsInMergeAndProvenance(t *testing.T) {
	dir := t.TempDir()
	params := testParams(dir)
	genOpts := rtpstream.GenOptions{
		Seed: 9, Tracks: 3, PacketsPerTrack: 1000, PayloadSize: 160,
		LossRate: 0.01, ReorderWindow: 32,
		ClockJumpTrack: 1, ClockJumpSeconds: 2, TailLoss: 2, TruncateTail: 1,
	}
	m, err := RunGen(context.Background(), params, genOpts)
	if err != nil {
		t.Fatalf("RunGen: %v", err)
	}
	if m.ProvenanceErrors == 0 {
		t.Fatal("clock jump not reported")
	}
	// The anomaly must appear in the provenance manifest as well.
	data, err := os.ReadFile(filepath.Join(params.OutDir, "provenance.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var head ManifestHeader
	line := data[:len(data)-1]
	if i := indexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if err := json.Unmarshal(line, &head); err != nil {
		t.Fatal(err)
	}
	if len(head.Anomalies) != m.ProvenanceErrors {
		t.Fatalf("manifest anomalies = %d, metrics = %d",
			len(head.Anomalies), m.ProvenanceErrors)
	}
	found := false
	for _, a := range head.Anomalies {
		if a.Kind == "drift" && a.Track == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("drift anomaly for track 1 missing: %+v", head.Anomalies)
	}
	if _, err := Verify(context.Background(), params.OutDir); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

func TestSequenceWrapStream(t *testing.T) {
	dir := t.TempDir()
	params := testParams(dir)
	genOpts := rtpstream.GenOptions{
		Seed: 5, Tracks: 2, PacketsPerTrack: 500, PayloadSize: 160,
		ReorderWindow: 16, SeqNearWrap: true, TailLoss: 2, TruncateTail: 2,
	}
	m, err := RunGen(context.Background(), params, genOpts)
	if err != nil {
		t.Fatalf("RunGen: %v", err)
	}
	if m.FramesWritten == 0 || m.PacketsLost == 0 {
		t.Fatalf("wrap run metrics = %+v", m)
	}
	if _, err := Verify(context.Background(), params.OutDir); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestLocatedErrors(t *testing.T) {
	// Empty input.
	params := testParams(t.TempDir())
	params.Packets = 0
	if _, err := Run(context.Background(), params); err == nil {
		t.Fatal("expected empty-input error")
	} else if fe, ok := err.(*fault.Error); !ok || fe.Offset != 0 {
		t.Fatalf("error not located: %v", err)
	}
	// Path traversal.
	params = testParams(t.TempDir())
	params.OutDir = "../evil"
	if _, err := Run(context.Background(), params); err == nil {
		t.Fatal("expected traversal error")
	}
	// Output space limit.
	params = testParams(t.TempDir())
	params.MaxOutBytes = 4096
	if _, err := Run(context.Background(), params); err == nil {
		t.Fatal("expected space-limit error")
	} else if fe, ok := err.(*fault.Error); !ok || fe.Kind != fault.KindResource {
		t.Fatalf("expected resource fault, got %v", err)
	}
}

func TestCancelCleansUp(t *testing.T) {
	dir := t.TempDir()
	params := testParams(dir)
	params.Packets = 120000 // long enough to cancel mid-run
	ctx, cancel := context.WithCancel(context.Background())
	before := runtime.NumGoroutine()
	done := make(chan error, 1)
	go func() {
		_, err := Run(ctx, params)
		done <- err
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected cancellation error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not stop after cancel")
	}
	// All goroutines must exit.
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if runtime.NumGoroutine() > before {
		t.Fatalf("goroutines leaked: before=%d now=%d", before, runtime.NumGoroutine())
	}
	// No temporary files may remain.
	leftover := []string{}
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && len(info.Name()) > 0 && info.Name()[0] == '.' {
			leftover = append(leftover, path)
		}
		return nil
	})
	if len(leftover) > 0 {
		t.Fatalf("temp files left behind: %v", leftover)
	}
}
