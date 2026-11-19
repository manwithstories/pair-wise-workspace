package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func invoke(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := run(args, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

func TestDemoExitAndSummary(t *testing.T) {
	out := filepath.Join(t.TempDir(), "report.json")
	code, stdout, stderr := invoke(t, "demo", "--seed", "42", "--packet-loss-burst", "4",
		"--bandwidth-kbps", "800,2400,600", "--out", out)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	for _, field := range []string{
		"roundtrip_consistency", "keyframe_hit_rate", "segment_count", "segment_sha256",
		"duplicate_packets", "reordered_packets", "burst_lost_packets", "errors_with_offset",
	} {
		if !strings.Contains(stdout, field) {
			t.Errorf("stdout missing %q", field)
		}
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("report not written: %v", err)
	}
	var report map[string]any
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("report is not valid JSON: %v", err)
	}
	if report["roundtrip_consistency"].(float64) < 0.99 {
		t.Errorf("roundtrip consistency %v below 0.99", report["roundtrip_consistency"])
	}
	if report["keyframe_hit_rate"].(float64) < 0.95 {
		t.Errorf("keyframe hit rate %v below 0.95", report["keyframe_hit_rate"])
	}
	hashes, _ := report["segment_sha256"].([]any)
	if len(hashes) == 0 {
		t.Error("segment_sha256 is empty")
	}
	for i, h := range hashes {
		s, _ := h.(string)
		if len(s) != 64 {
			t.Fatalf("segment %d hash %q is not 64 hex chars", i, s)
		}
	}
}

func TestHelpAndVersion(t *testing.T) {
	if code, out, _ := invoke(t, "version"); code != 0 || !strings.Contains(out, "mediasync") {
		t.Errorf("version: code=%d out=%q", code, out)
	}
	if code, out, _ := invoke(t, "help"); code != 0 || !strings.Contains(out, "usage") {
		t.Errorf("help: code=%d out=%q", code, out)
	}
	if code, _, errOut := invoke(t); code != 1 || !strings.Contains(errOut, "usage") {
		t.Errorf("no args: code=%d stderr=%q", code, errOut)
	}
	if code, _, _ := invoke(t, "bogus"); code != 1 {
		t.Errorf("unknown command: code=%d", code)
	}
}

func TestInvalidArguments(t *testing.T) {
	cases := [][]string{
		{"demo", "--seed", "-1"},
		{"demo", "--packet-loss-burst", "-1"},
		{"demo", "--packet-loss-burst", "1000"},
		{"demo", "--target-ms", "0"},
		{"demo", "--overlap-ms", "-1"},
		{"demo", "--overlap-ms", "1500"},
		{"demo", "--window", "0"},
		{"demo", "--bandwidth-kbps", "abc"},
		{"demo", "--bandwidth-kbps", "800,,2400"},
		{"demo", "--frames", "nonsense"},
		{"demo", "unexpected"},
	}
	for _, args := range cases {
		code, _, stderr := invoke(t, args...)
		if code != 1 {
			t.Errorf("%v: exit = %d, want 1 (stderr=%q)", args, code, stderr)
		}
		if stderr == "" {
			t.Errorf("%v: no diagnostic on stderr", args)
		}
	}
}

func TestOutputTraversalRejected(t *testing.T) {
	code, _, stderr := invoke(t, "demo", "--out", "../../tmp/escape.json")
	if code != 1 {
		t.Fatalf("traversal output accepted: code=%d", code)
	}
	if !strings.Contains(stderr, "write report") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestQuietMode(t *testing.T) {
	code, stdout, _ := invoke(t, "demo", "--quiet")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if stdout != "" {
		t.Errorf("quiet mode printed %q", stdout)
	}
}
