package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// runCLI executes the command and returns its exit code and streams.
func runCLI(args ...string) (int, string, string) {
	var out, errBuf bytes.Buffer
	code := run(args, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

func TestDemoPrintsAllAcceptanceFields(t *testing.T) {
	dir := t.TempDir()
	code, out, errOut := runCLI("demo", "--seed", "17",
		"--faults", "duplicate,reorder,burst-loss",
		"--bandwidth-mbps", "8,2,12",
		"--interrupt-after", "index", "--resume", "--dir", dir)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	for _, field := range []string{
		"target_bitrate_kbps", "actual_bitrate_kbps", "bitrate_delta_pct",
		"quality_psnr_db", "recovered_packets", "manifest_alignment_ms",
		"index_seek_mismatches", "manifest_sha256",
		"rollback_stage", "resume_stage", "injected_faults",
	} {
		if !strings.Contains(out, field) {
			t.Errorf("output missing %q:\n%s", field, out)
		}
	}
}

func TestJSONSummaryMatchesFields(t *testing.T) {
	dir := t.TempDir()
	code, out, errOut := runCLI("demo", "--seed", "17", "--json", "--dir", dir)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(out), &summary); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	for _, k := range []string{
		"target_bitrate_kbps", "actual_bitrate_kbps", "bitrate_delta_pct",
		"quality_psnr_db", "recovered_packets", "manifest_alignment_ms",
		"index_seek_mismatches", "manifest_sha256", "rollback_stage", "resume_stage",
	} {
		if _, ok := summary[k]; !ok {
			t.Errorf("summary missing key %q", k)
		}
	}
	if summary["index_seek_mismatches"].(float64) != 0 {
		t.Errorf("seek mismatches=%v", summary["index_seek_mismatches"])
	}
	if summary["manifest_alignment_ms"].(float64) > 1 {
		t.Errorf("alignment=%v ms", summary["manifest_alignment_ms"])
	}
}

func TestExitCodeInvalidInput(t *testing.T) {
	if code, _, _ := runCLI(); code != exitInvalid {
		t.Fatalf("no args exit=%d", code)
	}
	if code, _, _ := runCLI("bogus"); code != exitInvalid {
		t.Fatalf("unknown command exit=%d", code)
	}
	if code, _, _ := runCLI("demo", "--faults", "nonsense"); code != exitInvalid {
		t.Fatalf("bad fault exit=%d", code)
	}
	if code, _, _ := runCLI("demo", "--bandwidth-mbps", "zero"); code != exitInvalid {
		t.Fatalf("bad bandwidth exit=%d", code)
	}
	if code, _, _ := runCLI("demo", "--interrupt-after", "bogus", "--dir", t.TempDir()); code != exitInvalid {
		t.Fatalf("bad stage exit=%d", code)
	}
}

func TestExitCodeRecoveryWhenInterruptedWithoutResume(t *testing.T) {
	code, _, stderr := runCLI("demo", "--interrupt-after", "index", "--dir", t.TempDir())
	if code != exitRecovery {
		t.Fatalf("exit=%d want 3 (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stderr, "resume") {
		t.Fatalf("stderr should explain how to resume: %s", stderr)
	}
}

func TestFaultsReportNonZeroCounts(t *testing.T) {
	dir := t.TempDir()
	code, out, errOut := runCLI("demo", "--faults", "duplicate,reorder,burst-loss", "--json", "--dir", dir)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	var s map[string]any
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"recovered_packets", "duplicate_packets", "reordered_packets"} {
		if s[k].(float64) == 0 {
			t.Errorf("%s should be non-zero", k)
		}
	}
	if s["injected_faults"].(float64) == 0 {
		t.Errorf("injected_faults should be non-zero")
	}
}

func TestDeterministicAcrossRuns(t *testing.T) {
	run := func() map[string]any {
		code, out, errOut := runCLI("demo", "--seed", "17", "--faults", "burst-loss", "--json", "--dir", t.TempDir())
		if code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, errOut)
		}
		var s map[string]any
		if err := json.Unmarshal([]byte(out), &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	a, b := run(), run()
	if a["manifest_sha256"] != b["manifest_sha256"] {
		t.Fatalf("digest differs: %v vs %v", a["manifest_sha256"], b["manifest_sha256"])
	}
	for _, k := range []string{"actual_bitrate_kbps", "quality_psnr_db", "manifest_alignment_ms"} {
		if a[k] != b[k] {
			t.Errorf("%s differs: %v vs %v", k, a[k], b[k])
		}
	}
}
