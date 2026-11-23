package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func testOpts(dir, fixture string) demoOpts {
	return demoOpts{seed: 42, memoryMiB: 16, consumerDelayMs: 1,
		lossBurst: 5, fixture: fixture, workDir: dir}
}

func run(t *testing.T, opts demoOpts) (string, int) {
	t.Helper()
	var buf bytes.Buffer
	code := runDemo(opts, &buf)
	return buf.String(), code
}

func field(out, key string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, key+"=") {
			return strings.TrimPrefix(line, key+"=")
		}
	}
	return ""
}

func TestDemoSuccessMetrics(t *testing.T) {
	out, code := run(t, testOpts(t.TempDir(), "ok"))
	if code != 0 {
		t.Fatalf("exit=%d\n%s", code, out)
	}
	want := map[string]string{
		"av_sync_error_ms":   "0.8",
		"loss_recovery_rate": "0.80",
		"index_entries":      "15",
		"blocked_node":       "none",
	}
	for k, v := range want {
		if got := field(out, k); got != v {
			t.Fatalf("%s=%q want %q\n%s", k, got, v, out)
		}
	}
	var p95, peak int
	fmt.Sscanf(field(out, "p95_latency_ms"), "%d", &p95)
	fmt.Sscanf(field(out, "peak_memory_mib"), "%d", &peak)
	if p95 > 120 {
		t.Fatalf("p95=%d > 120ms", p95)
	}
	if peak > 16 {
		t.Fatalf("peak=%d MiB > 16", peak)
	}
	if len(field(out, "index_sha256")) != 64 {
		t.Fatalf("index_sha256 missing\n%s", out)
	}
}

func TestDemoDeterministicIndexDigest(t *testing.T) {
	out1, _ := run(t, testOpts(t.TempDir(), "ok"))
	out2, _ := run(t, testOpts(t.TempDir(), "ok"))
	if field(out1, "index_sha256") != field(out2, "index_sha256") {
		t.Fatalf("digest drift:\n%s\n%s", out1, out2)
	}
}

func TestDemoTruncatedFixture(t *testing.T) {
	out, code := run(t, testOpts(t.TempDir(), "truncated"))
	if code != 2 {
		t.Fatalf("exit=%d want 2\n%s", code, out)
	}
	if field(out, "blocked_node") != "index" {
		t.Fatalf("blocked_node=%s\n%s", field(out, "blocked_node"), out)
	}
	if field(out, "index_entries") != "0" {
		t.Fatal("truncated media must not commit index entries")
	}
	if field(out, "truncated_offset") == "" {
		t.Fatalf("truncated_offset missing\n%s", out)
	}
}
