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

	"mediapipe/internal/audit"
)

// runDemoCapture executes the demo with stdout captured.
func runDemoCapture(t *testing.T, args ...string) (string, int) {
	t.Helper()
	var buf bytes.Buffer
	stdout = &buf
	defer func() { stdout = os.Stdout }()
	code := run(append([]string{"demo"}, args...))
	return buf.String(), code
}

type parsedCase struct {
	fields map[string]string
	line   string
}

var caseLine = regexp.MustCompile(`^case=(\S+) `)

func parseCases(t *testing.T, out string) map[string]parsedCase {
	t.Helper()
	cases := make(map[string]parsedCase)
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "case=") {
			continue
		}
		fields := make(map[string]string)
		for _, tok := range strings.Fields(line) {
			k, v, ok := strings.Cut(tok, "=")
			if !ok {
				continue
			}
			fields[k] = v
		}
		name := fields["case"]
		if name == "" {
			t.Fatalf("unparsable case line: %q", line)
		}
		cases[name] = parsedCase{fields: fields, line: line}
	}
	return cases
}

func requireField(t *testing.T, c parsedCase, key string) string {
	t.Helper()
	v, ok := c.fields[key]
	if !ok {
		t.Fatalf("case %s missing %q in %q", c.fields["case"], key, c.line)
	}
	return v
}

func TestDemoPrintsAllRequiredFields(t *testing.T) {
	out, code := runDemoCapture(t, "--seed", "73", "--budget-mib", "32", "--slow-consumer", "1ms")
	if code != 0 {
		t.Fatalf("exit code=%d\n%s", code, out)
	}
	required := []string{
		"case", "target_bps", "actual_bps", "delta_pct", "payload_hash_mismatch",
		"crc_errors", "manifest_skew_ns", "buffer_peak_bytes", "replay_digest",
		"corrupt_offset", "resume_digest_equal",
	}
	cases := parseCases(t, out)
	if len(cases) < 4 {
		t.Fatalf("expected at least four cases, got %d\n%s", len(cases), out)
	}
	for name, c := range cases {
		for _, key := range required {
			if _, ok := c.fields[key]; !ok {
				t.Fatalf("case %s missing field %q", name, key)
			}
		}
	}
}

func TestDemoMeetsAcceptanceThresholds(t *testing.T) {
	out, code := runDemoCapture(t, "--seed", "73", "--budget-mib", "32", "--slow-consumer", "1ms")
	if code != 0 {
		t.Fatalf("exit=%d\n%s", code, out)
	}
	cases := parseCases(t, out)
	success, ok := cases["success"]
	if !ok {
		t.Fatalf("no success case\n%s", out)
	}
	delta, err := strconv.ParseFloat(requireField(t, success, "delta_pct"), 64)
	if err != nil {
		t.Fatal(err)
	}
	if delta < -0.5 || delta > 0.5 {
		t.Fatalf("delta_pct=%.4f outside ±0.5%%", delta)
	}
	if v := requireField(t, success, "payload_hash_mismatch"); v != "0" {
		t.Fatalf("payload_hash_mismatch=%s", v)
	}
	if v := requireField(t, success, "crc_errors"); v != "0" {
		t.Fatalf("crc_errors=%s", v)
	}
	skew, err := strconv.ParseInt(requireField(t, success, "manifest_skew_ns"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if skew > 1_000_000 {
		t.Fatalf("manifest_skew_ns=%d exceeds 1000000", skew)
	}
	peak, err := strconv.ParseInt(requireField(t, success, "buffer_peak_bytes"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if peak > 33_554_432 {
		t.Fatalf("buffer_peak_bytes=%d exceeds 33554432", peak)
	}
	if v := requireField(t, success, "replay_digest"); len(v) != 64 {
		t.Fatalf("replay_digest=%q is not a SHA-256 hex string", v)
	}
}

func TestDemoReplayDigestIsStable(t *testing.T) {
	out1, _ := runDemoCapture(t, "--seed", "73", "--budget-mib", "32", "--slow-consumer", "1ms")
	out2, _ := runDemoCapture(t, "--seed", "73", "--budget-mib", "32", "--slow-consumer", "1ms")
	c1 := parseCases(t, out1)
	c2 := parseCases(t, out2)
	for name := range c1 {
		a := requireField(t, c1[name], "replay_digest")
		b := requireField(t, c2[name], "replay_digest")
		if a != b {
			t.Fatalf("case %s replay digest changed: %q vs %q", name, a, b)
		}
	}
}

func TestDemoLateDiffersFromClean(t *testing.T) {
	out, _ := runDemoCapture(t, "--seed", "73", "--slow-consumer", "1ms")
	cases := parseCases(t, out)
	if requireField(t, cases["success"], "replay_digest") == requireField(t, cases["late"], "replay_digest") {
		t.Fatal("late fixture produced the same replay digest as clean")
	}
}

func TestDemoTruncatedReportsOffsetAndExit(t *testing.T) {
	out, _ := runDemoCapture(t, "--seed", "73", "--slow-consumer", "1ms")
	cases := parseCases(t, out)
	tr := cases["truncated"]
	if v := requireField(t, tr, "exit"); v != "2" {
		t.Fatalf("truncated exit=%s", v)
	}
	off, err := strconv.ParseInt(requireField(t, tr, "corrupt_offset"), 10, 64)
	if err != nil || off <= 0 {
		t.Fatalf("corrupt_offset=%d err=%v", off, err)
	}
}

func TestDemoResumeDigestEqual(t *testing.T) {
	out, _ := runDemoCapture(t, "--seed", "73", "--slow-consumer", "1ms")
	cases := parseCases(t, out)
	if v := requireField(t, cases["resume"], "resume_digest_equal"); v != "true" {
		t.Fatalf("resume_digest_equal=%s", v)
	}
	if v := requireField(t, cases["resume"], "exit"); v != "0" {
		t.Fatalf("resume exit=%s", v)
	}
}

func TestDemoUnsupportedMappingExitsThree(t *testing.T) {
	out, _ := runDemoCapture(t, "--seed", "73", "--slow-consumer", "1ms")
	cases := parseCases(t, out)
	if v := requireField(t, cases["unsupported"], "exit"); v != "3" {
		t.Fatalf("unsupported exit=%s", v)
	}
	if v := requireField(t, cases["beyond-horizon"], "exit"); v != "3" {
		t.Fatalf("beyond-horizon exit=%s", v)
	}
	// The interrupted case exercises exit code 4 for a recoverable write
	// failure; it is an expected failure and must not fail the demo.
	if v := requireField(t, cases["interrupted"], "exit"); v != "4" {
		t.Fatalf("interrupted exit=%s", v)
	}
}

func TestDemoBudgetIsRespected(t *testing.T) {
	// A tiny budget must still respect the byte ceiling.
	out, _ := runDemoCapture(t, "--seed", "73", "--budget-mib", "1", "--slow-consumer", "1ms")
	if !strings.HasPrefix(out, "case=") {
		t.Fatalf("no output\n%s", out)
	}
	for _, c := range parseCases(t, out) {
		peak, err := strconv.ParseInt(c.fields["buffer_peak_bytes"], 10, 64)
		if err != nil {
			continue
		}
		if peak > 1<<20 {
			t.Fatalf("peak %d exceeds 1 MiB budget", peak)
		}
	}
}

func TestDemoWorksWithExplicitWorkdir(t *testing.T) {
	dir := t.TempDir()
	out, code := runDemoCapture(t, "--seed", "99", "--workdir", dir, "--slow-consumer", "0s")
	if code != 0 {
		t.Fatalf("exit=%d\n%s", code, out)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("workdir removed: %v", err)
	}
}

func TestDemoSeedChangesReplayDigest(t *testing.T) {
	out73, _ := runDemoCapture(t, "--seed", "73", "--slow-consumer", "0s")
	out99, _ := runDemoCapture(t, "--seed", "99", "--slow-consumer", "0s")
	a := parseCases(t, out73)["success"]
	b := parseCases(t, out99)["success"]
	if requireField(t, a, "replay_digest") == requireField(t, b, "replay_digest") {
		t.Fatal("different seeds produced identical replay digests")
	}
}

func TestDemoAllTargetsRun(t *testing.T) {
	for _, target := range []string{"matroska", "webm", "fmp4"} {
		out, code := runDemoCapture(t, "--seed", "73", "--target", target, "--slow-consumer", "0s")
		if code != 0 {
			t.Fatalf("target %s exit=%d\n%s", target, code, out)
		}
		if !strings.Contains(out, "case=success") {
			t.Fatalf("target %s produced no success case", target)
		}
	}
}

func TestUnknownTargetRejected(t *testing.T) {
	out, code := runDemoCapture(t, "--target", "mkv2")
	if code != 3 {
		t.Fatalf("exit=%d out=%s", code, out)
	}
}

func TestHelpAndUnknownCommand(t *testing.T) {
	var buf bytes.Buffer
	stdout = &buf
	defer func() { stdout = os.Stdout }()
	if code := run(nil); code != 0 {
		t.Fatalf("no-arg exit=%d", code)
	}
	if code := run([]string{"help"}); code != 0 {
		t.Fatalf("help exit=%d", code)
	}
	if code := run([]string{"bogus"}); code != 1 {
		t.Fatalf("bogus exit=%d", code)
	}
}

func TestManifestWrittenWhenWorkdirGiven(t *testing.T) {
	dir := t.TempDir()
	out, code := runDemoCapture(t, "--seed", "73", "--workdir", dir, "--slow-consumer", "0s")
	if code != 0 {
		t.Fatalf("exit=%d\n%s", code, out)
	}
	path := filepath.Join(dir, "manifests", "success.json")
	m, err := readManifest(path)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if m.JobID == "" || m.Target != "matroska" {
		t.Fatalf("manifest missing identity: %+v", m)
	}
	if len(m.Tracks) != 3 {
		t.Fatalf("manifest tracks=%d", len(m.Tracks))
	}
	if m.OutputDigest == "" || m.ReplayDigest == "" {
		t.Fatal("manifest missing digests")
	}
	// The delivered artifact must exist and match the manifest's recorded size.
	artifact := filepath.Join(dir, "output", "success.mkv")
	info, err := os.Stat(artifact)
	if err != nil {
		t.Fatalf("delivered artifact missing: %v", err)
	}
	if info.Size() != m.OutputBytes {
		t.Fatalf("artifact %d bytes, manifest says %d", info.Size(), m.OutputBytes)
	}
	// The late case must also write a manifest with recomputed windows.
	lm, err := readManifest(filepath.Join(dir, "manifests", "late.json"))
	if err != nil {
		t.Fatalf("read late manifest: %v", err)
	}
	recomputed := 0
	for _, w := range lm.Windows {
		if w.Recomputed {
			recomputed++
		}
	}
	if recomputed == 0 {
		t.Fatal("late manifest has no recomputed windows")
	}
}

// readManifest loads a manifest written by the demo.
func readManifest(path string) (audit.Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return audit.Manifest{}, err
	}
	var m audit.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return audit.Manifest{}, err
	}
	return m, nil
}
