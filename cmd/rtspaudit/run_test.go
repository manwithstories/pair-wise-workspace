package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"rtspaudit/internal/report"
)

func TestRunCompareEndToEnd(t *testing.T) {
	out := filepath.Join(t.TempDir(), "cmp.json")
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"compare", "--frames", "24", "--seed", "7", "--out", out,
	}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var got report.CompareReport
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Delta != 0 || got.Verdict != "consistent" || got.TCPFrames != got.UDPFrames {
		t.Fatalf("compare=%+v", got)
	}
}

func TestRunCleanPull(t *testing.T) {
	out := filepath.Join(t.TempDir(), "clean.json")
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"pull", "--transport", "tcp", "--frames", "12", "--loss", "0",
		"--reorder", "0", "--dup", "0", "--out", out,
	}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var got report.PullReport
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.FramesReassembled != 12 || got.DroppedFrames != 0 || got.IllegalTransitions != 0 {
		t.Fatalf("report=%+v", got)
	}
}

func TestRunUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"pull", "--transport", "sctp"}, &stdout, &stderr)
	if code != exitUsage {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
}
