package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestMissingSegmentIsReencoded(t *testing.T) {
	job := t.TempDir()
	eng, err := NewEngine()
	if err != nil {
		t.Skipf("engine unavailable: %v", err)
	}
	cfg, paths := testConfig(t, job, 0)
	logger := newLogger(t, filepath.Join(t.TempDir(), "m.jsonl"))
	defer logger.Close()
	caps := parseCaps(t, paths.SRT)

	prime := cfg
	prime.InterruptAfterCuts = 2
	if _, err := eng.Run(context.Background(), prime, caps, logger); err == nil {
		t.Fatal("expected injected interrupt on prime run")
	}
	// Delete a committed segment entirely (missing, not merely corrupt).
	target := filepath.Join(job, "v1_001.m4s")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	recover := cfg
	recover.InterruptAfterCuts = 0
	res, err := eng.Run(context.Background(), recover, caps, logger)
	if err != nil {
		t.Fatalf("recovery after missing segment: %v", err)
	}
	if len(res.Journal.Segments) != 8 {
		t.Errorf("segments after recovery = %d, want 8", len(res.Journal.Segments))
	}
	if _, err := os.Stat(target); err != nil {
		t.Error("missing segment was not regenerated")
	}
}
