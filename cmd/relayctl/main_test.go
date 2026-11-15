package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"pairwise.local/relayctl/internal/signal"
)

func TestExitCodeMappings(t *testing.T) {
	if code := run([]string{"verify", "--signal", "0.0.0.0:47000"}, &bytes.Buffer{}, &bytes.Buffer{}); code != exitNonLoopback {
		t.Fatalf("expected exit 3, got %d", code)
	}
	if code := run([]string{"verify", "--egress", "9"}, &bytes.Buffer{}, &bytes.Buffer{}); code != exitConfig {
		t.Fatalf("expected exit 2, got %d", code)
	}
	if code := run([]string{"report", "--input", "/does/not/exist"}, &bytes.Buffer{}, &bytes.Buffer{}); code != exitConfig {
		t.Fatalf("expected report exit 2, got %d", code)
	}
	timeout := &signal.RemoteError{Code: signal.CodeHandshakeTimeout, Message: "timeout"}
	if code := codeForError(timeout); code != exitSignal {
		t.Fatalf("expected timeout exit 4, got %d", code)
	}
	if codeForError(nil) != exitOK {
		t.Fatal("nil error must map to success")
	}
}

func TestVerifyThresholdExitAndReport(t *testing.T) {
	out := filepath.Join(t.TempDir(), "report.json")
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"verify", "--signal", "127.0.0.1:0", "--duration", "1s", "--egress", "1",
		"--impair", "loss=1,seed=3", "--max-loss", "0", "--out", out,
	}, &stdout, &stderr)
	if code != exitThreshold {
		t.Fatalf("expected exit 5, got %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if strings.Count(stdout.String(), "\n") != 1 || !strings.Contains(stdout.String(), `"forwarded_packets":0`) {
		t.Fatalf("unexpected stdout: %s", stdout.String())
	}
	if code := run([]string{"report", "--input", out, "--format", "table"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("report failed: %s", stderr.String())
	}
}
