package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"mediamix/internal/clock"
	"mediamix/internal/rtp"
	"mediamix/internal/txn"
)

// TestReplayFailureOnTamperedCommit proves that once a committed manifest no
// longer matches its WAL payload, replay fails with a recoverable-class
// error rather than silently delivering wrong bytes. This is the condition
// the CLI maps to exit code 3.
func TestReplayFailureOnTamperedCommit(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig(dir)
	cfg.QueueBytes = 1 << 20
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	fx := rtp.Generate(rtp.DefaultStreamConfig(), rtp.FixtureConfig{
		Seed: 41, Seconds: 2, LossRate: 0.02, SenderReportEvery: 25,
	})
	if _, err := r.Run(context.Background(), fx); err != nil {
		t.Fatalf("clean run: %v", err)
	}

	// Tamper with a committed manifest so its stored SHA no longer matches
	// the WAL payload. Reopen and replay must reject it.
	entries, _ := os.ReadDir(dir)
	var manifest string
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".commit" {
			manifest = filepath.Join(dir, e.Name())
			break
		}
	}
	if manifest == "" {
		t.Fatal("no committed manifest found")
	}
	raw, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte(nil), raw...)
	// Flip a hex digit inside the SHA256 field region.
	for i, b := range tampered {
		if b == 'a' {
			tampered[i] = 'b'
			break
		}
	}
	if err := os.WriteFile(manifest, tampered, 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := txn.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer c.Close()
	_, err = c.Replay()
	if err == nil {
		t.Fatal("tampered manifest must fail replay")
	}
	if !clock.IsCode(err, clock.ErrReplay) {
		t.Fatalf("expected replay failure code, got %v", err)
	}
}

// TestCorruptWALTailSurvivesOpen proves a garbage tail is truncated and the
// committed prefix still replays successfully (the recovery-success path).
func TestCorruptWALTailSurvivesOpen(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig(dir)
	cfg.QueueBytes = 1 << 20
	r, _ := New(cfg)
	fx := rtp.Generate(rtp.DefaultStreamConfig(), rtp.FixtureConfig{
		Seed: 9, Seconds: 2, SenderReportEvery: 25,
	})
	if _, err := r.Run(context.Background(), fx); err != nil {
		t.Fatal(err)
	}
	walPath := filepath.Join(dir, "mediamix.wal")
	info, _ := os.Stat(walPath)
	before := info.Size()

	f, err := os.OpenFile(walPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte("###CORRUPT-TAIL-BYTES###"))
	f.Close()

	c, dropped, err := txn.OpenWAL(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer c.Close()
	if dropped == 0 {
		t.Fatal("expected corrupt tail to be reported")
	}
	info2, _ := os.Stat(walPath)
	if info2.Size() != before {
		t.Fatalf("wal truncated size %d != %d", info2.Size(), before)
	}
}
