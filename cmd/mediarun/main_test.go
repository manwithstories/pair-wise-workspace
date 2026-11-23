package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildBinary compiles the command once for the integration tests.
func buildBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "mediarun")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	return bin
}

func TestDemoInvariantsAndExitZero(t *testing.T) {
	if testing.Short() {
		t.Skip("integration demo skipped in short mode")
	}
	bin := buildBinary(t)
	out, err := exec.Command(bin, "demo", "--seed", "42",
		"--fault", "reorder,truncate,drift", "--resume").CombinedOutput()
	if err != nil {
		t.Fatalf("demo failed: %v\n%s", err, out)
	}
	text := string(out)
	for _, want := range []string{
		"roundtrip_rate=1.0000",
		"keyframe_hit_rate=1.0000",
		"segment_hash_match=1.000",
		"resume_hash_equal=true",
		"provenance_complete=true",
		"invariants=ok",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("output missing %q:\n%s", want, text)
		}
	}
	if !strings.Contains(text, "recovered_gaps=") ||
		!strings.Contains(text, "rollback_count=") {
		t.Fatalf("output missing gap or rollback counters:\n%s", text)
	}
}

func TestValidateRejectsBadPlaylist(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.m3u8")
	if err := osWrite(bad, "#EXTM3U\n#EXTINF:4,\n../escape.m4s\n"); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "validate", "--playlist", bad)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("bad playlist accepted:\n%s", out)
	}
	if code := exitCode(err); code != exitRejected {
		t.Fatalf("exit code = %d, want %d\n%s", code, exitRejected, out)
	}
}

func TestValidateAcceptsGoodPlaylist(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	good := filepath.Join(dir, "good.m3u8")
	body := "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\nseg/a.m4s\n#EXTINF:4,\nseg/b.m4s\n"
	if err := osWrite(good, body); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(bin, "validate", "--playlist", good).CombinedOutput()
	if err != nil {
		t.Fatalf("good playlist rejected: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "accepted") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}
