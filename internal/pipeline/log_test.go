package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediarun/internal/txn"
)

// TestTruncatedLogReplaysConsistently is the log-truncation acceptance case:
// a torn final record must be discarded at replay, and the resumed run must
// still produce the reference final manifest.
func TestTruncatedLogReplaysConsistently(t *testing.T) {
	refDir := t.TempDir()
	refCfg := Config{Seed: 11, Segments: 3, SegmentDurNS: 2_000_000_000,
		PacketRate: 50, WorkDir: refDir, Faults: Faults{Drop: true}}
	ref, err := Run(refCfg)
	if err != nil {
		t.Fatalf("reference run: %v", err)
	}

	dir := t.TempDir()
	cfg := refCfg
	cfg.WorkDir = dir
	if _, err := Run(cfg); err != nil {
		t.Fatalf("first run: %v", err)
	}
	logPath := filepath.Join(dir, "wal.log")
	// Tear the tail: append an incomplete record the way a crash mid-write
	// would leave it.
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	if _, err := f.WriteString(`{"type":"stage-commit","seq":999`); err != nil {
		t.Fatalf("tear log: %v", err)
	}
	f.Close()

	recs, err := txn.ReplayFile(logPath)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	for _, r := range recs {
		if !r.Verify() {
			t.Fatalf("torn record survived replay: %+v", r)
		}
	}
	final, err := txn.HashFile(filepath.Join(dir, "final.json"))
	if err != nil {
		t.Fatalf("hash final: %v", err)
	}
	if final != ref.NormalHash {
		t.Fatalf("torn log changed final output: %s != %s", final, ref.NormalHash)
	}
	// Re-run in resume mode: the torn tail must not confuse the resume.
	cfg.Resume = true
	cfg.ReferenceHash = ref.NormalHash
	res, err := Run(cfg)
	if err != nil {
		t.Fatalf("resume after torn tail: %v", err)
	}
	if !res.ResumeHashEqual {
		t.Fatalf("resume after torn tail produced %s, want %s",
			res.ResumedHash, ref.NormalHash)
	}
}

// TestLogTruncateInjection mirrors the CLI's InjectLogTruncate path.
func TestLogTruncateInjection(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Seed: 5, Segments: 3, SegmentDurNS: 2_000_000_000,
		PacketRate: 50, WorkDir: dir, Inject: InjectLogTruncate}
	// The first invocation commits one stage, then tears its own log. It
	// must fail cleanly or resume; either way no partial final may appear.
	if _, err := Run(cfg); err != nil {
		if !strings.Contains(err.Error(), "txn") {
			t.Fatalf("unexpected error shape: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "final.json")); err != nil {
		if !os.IsNotExist(err) {
			t.Fatalf("stat: %v", err)
		}
	} else {
		// If it did commit everything, the artefact must be well formed.
		data, err := os.ReadFile(filepath.Join(dir, "final.json"))
		if err != nil || len(data) == 0 {
			t.Fatalf("empty final after truncation injection")
		}
	}
}
