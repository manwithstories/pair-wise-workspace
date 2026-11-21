package main

import (
	"os"
	"path/filepath"
	"testing"

	"mediascan/internal/fixture"
)

// TestCommandExitCodes drives the CLI entry points directly so the contract is
// locked without depending on `go run`'s wrapper status.
func TestCommandExitCodes(t *testing.T) {
	work := filepath.Join(t.TempDir(), "work")
	if code := run([]string{"demo", "--work", work, "--loss-every", "17", "--interrupt-after", "3"}); code != exitOK {
		t.Fatalf("demo exit=%d", code)
	}
	if code := run([]string{"resume", "--work", work}); code != exitOK {
		t.Fatalf("resume exit=%d", code)
	}
	if code := run([]string{"verify", "--work", work}); code != exitOK {
		t.Fatalf("verify exit=%d", code)
	}
}

func TestUsageExitsOne(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"bogus"},
		{"demo"},
		{"demo", "--work", t.TempDir(), "--loss-every", "1"},
		{"resume", "--work", t.TempDir(), "--interrupt-after", "-2"},
	} {
		if code := run(args); code != exitUsage {
			t.Errorf("run(%v)=%d, want %d", args, code, exitUsage)
		}
	}
}

func TestVerifyMismatchExitsTwo(t *testing.T) {
	work := filepath.Join(t.TempDir(), "work")
	if code := run([]string{"demo", "--work", work}); code != exitOK {
		t.Fatalf("demo exit=%d", code)
	}
	prov := filepath.Join(work, "report", "provenance.jsonl")
	data, err := os.ReadFile(prov)
	if err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte(`{"kind":"summary","input_sha256":"deadbeef","result_sha256":"cafe"}`+"\n"), data...)
	if err := os.WriteFile(prov, tampered, 0o644); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"verify", "--work", work}); code != exitMismatch {
		t.Fatalf("tampered verify exit=%d, want %d", code, exitMismatch)
	}
}

func TestDemoIsSelfContained(t *testing.T) {
	work := filepath.Join(t.TempDir(), "work")
	if code := run([]string{"demo", "--work", work}); code != exitOK {
		t.Fatal("demo failed")
	}
	for _, rel := range []string{
		"hls/media.m3u8", "dash/manifest.mpd", "media/init-v.mp4", "rtp/capture.rtp",
		"checkpoint.json", "index.json", "report/provenance.jsonl",
	} {
		if _, err := os.Stat(filepath.Join(work, rel)); err != nil {
			t.Errorf("demo did not produce %s: %v", rel, err)
		}
	}
	if _, err := os.Stat(fixture.SegmentPath(work, fixture.NewLayout(work).MissingIdx)); !os.IsNotExist(err) {
		t.Errorf("missing segment unexpectedly present: %v", err)
	}
}
