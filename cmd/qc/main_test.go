package main

import (
	"bytes"
	"strings"
	"testing"
)

func execute(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestCLINormalSuccessPrintsRequiredKeys(t *testing.T) {
	code, out, errOut := execute(t, "demo", "--profile", "normal", "--codec", "opus", "--seed", "41")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	for _, key := range []string{
		"packets=", "duplicates=", "recovered=", "recoverable_loss=", "recovery_rate=",
		"av_sync_p95_ms=", "e2e_p95_ms=", "first_blocked=", "output_sha256=", "status=ok",
	} {
		if !strings.Contains(out, key) {
			t.Fatalf("missing %q in:\n%s", key, out)
		}
	}
}

func TestCLIBurstFailOnceShowsRetryAndBlocker(t *testing.T) {
	code, out, errOut := execute(t, "demo", "--profile", "burst", "--codec", "aac", "--seed", "41", "--fail-once", "audio.repair")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	for _, want := range []string{
		"result=failed error_type=internal",
		"blocked_dependency=audio.repair",
		"result=retry_success",
		"first_blocked=audio.repair",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestCLICorruptExitTwo(t *testing.T) {
	code, out, _ := execute(t, "demo", "--profile", "corrupt", "--seed", "41")
	if code != 2 {
		t.Fatalf("code=%d want 2", code)
	}
	if !strings.Contains(out, "malformed=1") || !strings.Contains(out, "error_type=malformed") {
		t.Fatalf("corrupt output missing classification:\n%s", out)
	}
}

func TestCLIUsageAndUnknownProfile(t *testing.T) {
	if code, _, _ := execute(t); code != 1 {
		t.Fatalf("empty args code=%d want1", code)
	}
	code, out, _ := execute(t, "demo", "--profile", "nope", "--seed", "1")
	if code != 1 || !strings.Contains(out, "error_type=internal") {
		t.Fatalf("unknown profile code=%d out=%s", code, out)
	}
}
