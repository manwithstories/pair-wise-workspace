package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"relaycheck/internal/media"
)

func testConfig(out string) Config {
	c := DefaultConfig()
	c.Out = out
	return c
}

func TestNormalRun120SegmentsAndReport(t *testing.T) {
	res, f := Run(testConfig(t.TempDir()))
	if f != nil {
		t.Fatal(f)
	}
	r := res.Report
	if r.HashMatched != 120 || r.RoundtripMatched != 120 || r.KeyHits != 120 {
		t.Fatalf("hash=%d/120 rt=%d/120 key=%d/120", r.HashMatched, r.RoundtripMatched, r.KeyHits)
	}
	if len(r.Hashes) != 120 {
		t.Fatalf("hashes=%d", len(r.Hashes))
	}
	out := FormatResult(res)
	for _, want := range []string{
		"roundtrip_consistency=100.00%",
		"keyframe_hit_rate=100.00%（120/120）",
		"segment_hash_match=100.00%（120 片段）",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("report missing %q", want)
		}
	}
	if n := strings.Count(out, "sha256="); n != 120 {
		t.Fatalf("sha lines=%d want 120", n)
	}
}

func TestCrashExit75AndResumeMatchesUninterrupted(t *testing.T) {
	// Uninterrupted reference run.
	normalDir := filepath.Join(t.TempDir(), "ok")
	normal, f := Run(testConfig(normalDir))
	if f != nil {
		t.Fatal(f)
	}

	// Crashed run.
	recDir := filepath.Join(t.TempDir(), "recover")
	crashCfg := testConfig(recDir)
	crashCfg.CrashAfterSegment = 3
	_, f = Run(crashCfg)
	if f == nil || f.Code != media.CodeInterrupted || f.ExitCode() != 75 {
		t.Fatalf("want interrupted exit75, got %v", f)
	}
	if _, err := os.Stat(filepath.Join(recDir, "segments", "seg-000003.rlc")); !os.IsNotExist(err) {
		t.Fatal("segment 3 must not be committed at crash point")
	}

	// Resume with the same parameters plus --resume.
	resumeCfg := testConfig(recDir)
	resumeCfg.CrashAfterSegment = 3
	resumeCfg.Resume = true
	res, f := Run(resumeCfg)
	if f != nil {
		t.Fatal(f)
	}
	if res.WALReplay != 1 {
		t.Fatalf("wal_replay=%d want 1", res.WALReplay)
	}
	if res.ResumeMatched != 120 || res.Report.HashMatched != 120 {
		t.Fatalf("resume=%d/120 hash=%d/120", res.ResumeMatched, res.Report.HashMatched)
	}
	for i := 0; i < 120; i++ {
		if res.Report.Hashes[i] != normal.Report.Hashes[i] {
			t.Fatalf("hash mismatch at segment %d", i)
		}
	}
	out := FormatResult(res)
	if !strings.Contains(out, "wal_replay=1") || !strings.Contains(out, "resume_hash_match=100.00%") {
		t.Fatalf("resume report missing replay/match lines:\n%s", out)
	}
}

func TestTruncatedMP4FaultAndNoOutputDir(t *testing.T) {
	out := filepath.Join(t.TempDir(), "relay-bad")
	cfg := testConfig(out)
	cfg.Case = "truncated-mp4"
	_, f := Run(cfg)
	if f == nil || f.Code != media.CodeParseError || f.TrackID != 1 || f.Range == nil {
		t.Fatalf("corrupt fault malformed: %+v", f)
	}
	if f.ExitCode() != 2 {
		t.Fatalf("exit=%d want 2", f.ExitCode())
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("invalid input must not create a committed output directory")
	}
	report := FormatFault(f)
	for _, token := range []string{"parse_error", "track_id=1", "time_range="} {
		if !strings.Contains(report, token) {
			t.Fatalf("fault report missing %q:\n%s", token, report)
		}
	}
}

func TestAnomalyCasesExit2(t *testing.T) {
	for _, name := range []string{"illegal-rotation", "bad-crop", "unknown-color"} {
		cfg := testConfig(filepath.Join(t.TempDir(), name))
		cfg.Case = name
		_, f := Run(cfg)
		if f == nil || f.ExitCode() != 2 {
			t.Fatalf("case %s: want exit2 fault, got %v", name, f)
		}
	}
}
