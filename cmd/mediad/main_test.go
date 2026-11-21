package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/mediamend/mediamend/internal/pairflow"
)

// acceptanceKeys is exactly what the task specification requires the
// acceptance run to print.
var acceptanceKeys = []string{
	"roundtrip_rate=",
	"keyframe_hit_rate=",
	"segment_sha256_checked=",
	"alignment_error_ticks=",
	"peak_inflight_frames=",
	"corrupt_records=",
	"first_error_offset=",
}

// parseKV turns "a=1\nb=2" into a map.
func parseKV(s string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			m[k] = v
		}
	}
	return m
}

func mustFloat(t *testing.T, kv map[string]string, key string) float64 {
	t.Helper()
	v, ok := kv[key]
	if !ok {
		t.Fatalf("missing key %q in %v", key, kv)
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		t.Fatalf("%s=%q is not a number: %v", key, v, err)
	}
	return f
}

// TestAcceptanceRun drives the documented acceptance flow at the library and
// command level in one process.
func TestAcceptanceRun(t *testing.T) {
	fixtureDir := filepath.Join(t.TempDir(), "fixture")
	m, faults := pairflow.Generate(pairflow.Options{
		Root: fixtureDir, Frames: 600, CorruptEvery: 137, CameraCount: 6,
	})
	if faults.Fatal() {
		t.Fatalf("fixture: %v", faults)
	}
	outDir := filepath.Join(t.TempDir(), "out")

	var stdout bytes.Buffer
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	code := Main([]string{
		"-input", fixtureDir, "-queue", "8", "-slow-consumer", "1ms",
		"-bitrates", "360p,720p,1080p", "-out", outDir, "-random-seeks", "150",
	})
	w.Close()
	os.Stdout = oldStdout
	if _, err := stdout.ReadFrom(r); err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit=%d output:\n%s", code, stdout.String())
	}
	kv := parseKV(stdout.String())
	for _, k := range acceptanceKeys {
		key := strings.TrimSuffix(k, "=")
		if _, ok := kv[key]; !ok {
			t.Fatalf("missing acceptance key %q in:\n%s", key, stdout.String())
		}
	}

	rt := mustFloat(t, kv, "roundtrip_rate")
	if !regexp.MustCompile(`^0\.99\d+$`).MatchString(kv["roundtrip_rate"]) {
		t.Fatalf("roundtrip_rate=%v must print as 0.99x", rt)
	}
	if kh := mustFloat(t, kv, "keyframe_hit_rate"); kh != 1.0 {
		t.Fatalf("keyframe_hit_rate=%v want 1.000", kh)
	}
	if sha := mustFloat(t, kv, "segment_sha256_checked"); sha <= 0 {
		t.Fatalf("segment_sha256_checked=%v", sha)
	}
	if al := mustFloat(t, kv, "alignment_error_ticks"); al != 0 {
		t.Fatalf("alignment_error_ticks=%v want 0", al)
	}
	if peak := mustFloat(t, kv, "peak_inflight_frames"); peak > 24 {
		t.Fatalf("peak_inflight_frames=%v exceeds 24", peak)
	}
	if corrupt := mustFloat(t, kv, "corrupt_records"); int(corrupt) != m.TotalCorrupt() {
		t.Fatalf("corrupt_records=%v want %d", corrupt, m.TotalCorrupt())
	}
	if off := mustFloat(t, kv, "first_error_offset"); off < 0 {
		t.Fatalf("first_error_offset=%v", off)
	}

	// The report file must agree with the terminal keys.
	data, err := os.ReadFile(filepath.Join(outDir, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("report.json invalid: %v", err)
	}
	if int(report["corrupt_records"].(float64)) != m.TotalCorrupt() {
		t.Fatalf("report corrupt_records=%v", report["corrupt_records"])
	}
	if report["alignment_error_ticks"].(float64) != 0 {
		t.Fatalf("report alignment=%v", report["alignment_error_ticks"])
	}
}

func TestAcceptanceExitCodes(t *testing.T) {
	// Invalid parameters -> 2.
	if code := Main([]string{"-bitrates", "360p,4k"}); code != 2 {
		t.Fatalf("bad bitrate exit=%d want 2", code)
	}
	if code := Main([]string{}); code != 2 {
		t.Fatalf("missing args exit=%d want 2", code)
	}
	if code := Main([]string{"-input", "x", "-out", "y", "-queue", "0"}); code != 2 {
		t.Fatalf("bad queue exit=%d want 2", code)
	}
	// Missing input directory -> 1.
	if code := Main([]string{"-input", t.TempDir(), "-out", t.TempDir()}); code != 1 {
		t.Fatalf("missing fixture exit=%d want 1", code)
	}
}

func TestSelfCheckBoundariesExitZero(t *testing.T) {
	dir := t.TempDir()
	if _, faults := pairflow.Generate(pairflow.Options{Root: dir, Frames: 90, CameraCount: 1}); faults.Fatal() {
		t.Fatalf("fixture: %v", faults)
	}
	var stdout bytes.Buffer
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	code := Main([]string{"-self-check-boundaries", "-input", dir})
	w.Close()
	os.Stdout = oldStdout
	stdout.ReadFrom(r)
	if code != 0 {
		t.Fatalf("self-check exit=%d output=%s", code, stdout.String())
	}
	got := strings.TrimSpace(stdout.String())
	want := "truncated=1 checksum_mismatch=1 timestamp_wrap=1 duplicate=1"
	// The JSON line (if any) precedes the summary; the summary must be the
	// final line exactly as specified.
	lines := strings.Split(got, "\n")
	if lines[len(lines)-1] != want {
		t.Fatalf("self-check output=%q want %q", got, want)
	}
}

func TestSelfCheckBoundariesMissingDir(t *testing.T) {
	var stderr bytes.Buffer
	oldErr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	code := Main([]string{"-self-check-boundaries", "-input", filepath.Join(t.TempDir(), "nope")})
	w.Close()
	os.Stderr = oldErr
	stderr.ReadFrom(r)
	if code != 1 {
		t.Fatalf("exit=%d want 1", code)
	}
}
