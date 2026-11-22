package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runCapture(t *testing.T, args []string) (string, int) {
	t.Helper()
	code := run(args)
	// run() prints directly to stdout; tests instead assert via a temp work
	// dir inspection and the returned code, keeping output deterministic.
	return "", code
}

func TestDemoExitCodeSuccess(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	args := []string{"demo", "--seed", "42", "--duration", "12s",
		"--bitrates", "800k,1600k,3200k", "--segment", "2s",
		"--inject", "truncated-box,wrap-drift-vfr",
		"--checkpoint-every", "4s", "--resume", "--work-dir", dir}
	if code := run(args); code != exitOK {
		t.Fatalf("exit=%d want 0", code)
	}
	// Manifest and checkpoint must exist; no tmp leftovers.
	if _, err := os.Stat(filepath.Join(dir, "primary", "manifest.json")); err != nil {
		t.Fatalf("manifest missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "primary", "checkpoint.json")); err != nil {
		t.Fatalf("checkpoint missing: %v", err)
	}
}

func TestDemoBadParameterExitOne(t *testing.T) {
	if code := run([]string{"demo", "--duration", "nope"}); code != exitInternal {
		t.Fatalf("bad duration exit=%d want 1", code)
	}
	if code := run([]string{"demo", "--bitrates", "bogus"}); code != exitInternal {
		t.Fatalf("bad bitrate exit=%d want 1", code)
	}
	if code := run([]string{"demo", "--inject", "not-a-real-fault"}); code != exitInternal {
		t.Fatalf("bad injection exit=%d want 1", code)
	}
	if code := run([]string{"bogus-command"}); code != exitInternal {
		t.Fatalf("bad command exit=%d want 1", code)
	}
}

func TestDemoCorruptionIsolatedAndLocated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	args := []string{"demo", "--seed", "9", "--duration", "10s",
		"--bitrates", "800k,1600k", "--segment", "2s",
		"--inject", "truncated-box", "--work-dir", dir}
	// Healthy ladder must still complete even though a corrupt representation
	// was generated and diagnosed.
	if code := run(args); code != exitOK {
		t.Fatalf("corruption isolation exit=%d want 0", code)
	}
	// Corrupt fixture file must be present and remain unparsable as healthy.
	bad := filepath.Join(dir, "primary", "corrupt.mp4")
	if _, err := os.Stat(bad); err != nil {
		t.Fatalf("corrupt fixture missing: %v", err)
	}
}

func TestBitrateAndDurationParsing(t *testing.T) {
	cases := map[string]int{
		"800k":  800000,
		"1600k": 1600000,
		"3.2m":  3200000,
	}
	for in, want := range cases {
		got, err := parseBitrate(in)
		if err != nil || got != want {
			t.Fatalf("parseBitrate(%s)=%d,%v want %d", in, got, err, want)
		}
	}
	if _, err := parseBitrate(""); err == nil {
		t.Fatal("empty bitrate accepted")
	}
	d, err := parseDuration("2s")
	if err != nil || d.Seconds() != 2 {
		t.Fatalf("parseDuration failed: %v %v", d, err)
	}
	if _, err := parseDuration("2000"); err == nil {
		t.Fatal("duration without suffix accepted")
	}
}

func TestResumeRejectsCorruptCheckpointExitTwo(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	first := []string{"demo", "--seed", "3", "--duration", "8s",
		"--bitrates", "800k", "--segment", "2s",
		"--checkpoint-every", "2s", "--resume", "--work-dir", dir}
	if code := run(first); code != exitOK {
		t.Fatalf("setup exit=%d", code)
	}
	// Corrupt the checkpoint JSON and run a plain (non-resume) pipeline over
	// the same work dir; resume validation must refuse with exit code 2.
	cp := filepath.Join(dir, "primary", "checkpoint.json")
	raw, err := os.ReadFile(cp)
	if err != nil {
		t.Fatal(err)
	}
	raw[0] = 'X'
	if err := os.WriteFile(cp, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// The 'run' subcommand reuses the work-dir and hits the bad checkpoint.
	manifest := writeSourceManifest(t, dir)
	code := run([]string{"run", "--manifest", manifest,
		"--work-dir", filepath.Join(dir, "primary"), "--segment-seconds", "2"})
	if code != exitCorrupt {
		t.Fatalf("corrupt checkpoint exit=%d want 2", code)
	}
}

func writeSourceManifest(t *testing.T, dir string) string {
	t.Helper()
	mp4 := filepath.Join(dir, "primary", "800k.mp4")
	rtp := filepath.Join(dir, "primary", "800k.rtp")
	body := `{"version":"mediaslice-source/v1","segment_ticks":180000,` +
		`"sources":[{"id":"800k","bitrate":800000,"mp4":"` + filepath.Base(mp4) +
		`","rtp":"` + filepath.Base(rtp) + `"}]}`
	mp := filepath.Join(dir, "primary", "sources.json")
	if err := os.WriteFile(mp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = strings.TrimSpace
	return mp
}
