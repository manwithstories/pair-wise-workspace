package main

import (
	"bytes"
	"strings"
	"testing"
)

func runDemo(t *testing.T, args ...string) (string, int) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := run(append([]string{"demo"}, args...), &out, &errBuf)
	return out.String(), code
}

func TestDemoDefault(t *testing.T) {
	out, code := runDemo(t, "--seed", "41", "--budget-mib", "32")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{
		"fixture=generated", "segments=12", "repaired=1", "unrecoverable=1",
		"index_entries=10", "target_bitrate_kbps=2400", "actual_bitrate_kbps=2280",
		"bitrate_delta_pct=-5.00", "quality_psnr_db=38.2", "manifest_error_ms=11.1",
		"budget_mib=32",
	} {
		if !strings.Contains(out, want+"\n") {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	// Peak memory must stay within budget.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "peak_memory_mib=") {
			if line == "peak_memory_mib=32" || strings.HasSuffix(line, "33") {
				t.Fatalf("peak memory at budget: %s", line)
			}
		}
	}
}

func TestDemoDeterministic(t *testing.T) {
	a, _ := runDemo(t, "--seed", "41", "--budget-mib", "32")
	b, _ := runDemo(t, "--seed", "41", "--budget-mib", "32")
	if a != b {
		t.Fatalf("non-deterministic:\n%s\nvs\n%s", a, b)
	}
}

func TestDemoTruncated(t *testing.T) {
	out, code := runDemo(t, "--case", "truncated", "--seed", "41", "--budget-mib", "32")
	if code != 2 {
		t.Fatalf("exit %d want 2", code)
	}
	for _, want := range []string{"recovered_segments=4", "rejected_packets=4", "alignment_error_ms=80.0"} {
		if !strings.Contains(out, want+"\n") {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestDemoBudgetExceeded(t *testing.T) {
	if _, code := runDemo(t, "--seed", "41", "--budget-mib", "1"); code != 1 {
		t.Fatalf("exit %d want 1", code)
	}
	if _, code := runDemo(t, "--seed", "41", "--budget-mib", "0"); code != 1 {
		t.Fatalf("exit %d want 1", code)
	}
}
