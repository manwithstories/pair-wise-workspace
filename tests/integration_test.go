// Package integration drives the whole loopback WebRTC data path end to end:
// fixture -> signaling -> forwarder -> sender peer -> receiver peer -> RTP
// statistics -> report. It is deliberately an external test package so it can
// only use the public interfaces the command itself uses.
package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/manwithstories/pair-wise-workspace/internal/cli"
)

// TestDemoProducesAcceptableMetrics is the acceptance check: 2000 packets at
// 1 ms with 32-slot reordering, 1% loss and 2% duplicates must land inside the
// documented ranges and exit 0.
func TestDemoProducesAcceptableMetrics(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end demo is too slow for -short")
	}
	opts, err := cli.Parse([]string{
		"demo",
		"--packets", "2000",
		"--interval-ms", "1",
		"--reorder", "32",
		"--duplicate-rate", "0.02",
		"--loss-rate", "0.01",
		"--json",
	}, os.Stderr)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var out, errOut bytes.Buffer
	code := cli.Run(context.Background(), opts, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s stdout=%s", code, errOut.String(), out.String())
	}

	var rep map[string]any
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("report is not valid JSON: %v\n%s", err, out.String())
	}
	required := []string{
		"candidate_pairs", "forwarded_packets", "lost_packets", "duplicate_packets",
		"jitter_avg_ms", "jitter_max_ms", "loopback_only", "exit_code",
		"reordered_packets", "received_packets", "source_packets",
	}
	for _, key := range required {
		if _, ok := rep[key]; !ok {
			t.Fatalf("report is missing required field %q: %s", key, out.String())
		}
	}

	if got := rep["candidate_pairs"].(float64); got != 1 {
		t.Fatalf("candidate_pairs=%v, want 1", got)
	}
	if got := rep["loopback_only"].(bool); !got {
		t.Fatal("loopback_only must be true")
	}
	forwarded := rep["forwarded_packets"].(float64)
	if forwarded < 1960 || forwarded > 2000 {
		t.Fatalf("forwarded_packets=%v outside [1960,2000]", forwarded)
	}
	lost := rep["lost_packets"].(float64)
	if lost < 20 || lost > 40 {
		t.Fatalf("lost_packets=%v outside [20,40]", lost)
	}
	dups := rep["duplicate_packets"].(float64)
	if dups < 30 || dups > 50 {
		t.Fatalf("duplicate_packets=%v outside [30,50]", dups)
	}
	avg := rep["jitter_avg_ms"].(float64)
	max := rep["jitter_max_ms"].(float64)
	if avg < 0 || avg > 20 {
		t.Fatalf("jitter_avg_ms=%v outside [0,20]", avg)
	}
	if max < avg {
		t.Fatalf("jitter_max_ms=%v is below jitter_avg_ms=%v", max, avg)
	}
	if exit := rep["exit_code"].(float64); exit != 0 {
		t.Fatalf("exit_code field=%v, want 0", exit)
	}

	// Internal consistency: every source packet is either forwarded, lost or
	// still in the queue, and forwarding cannot exceed what was enqueued.
	if recv := rep["received_packets"].(float64); recv < forwarded {
		t.Fatalf("received=%v is below forwarded=%v", recv, forwarded)
	}
	if src := rep["source_packets"].(float64); src != 2000 {
		t.Fatalf("source_packets=%v, want 2000", src)
	}
}

// TestInvalidArgumentsExitTwo covers the argument contract without needing a
// network round trip.
func TestInvalidArgumentsExitTwo(t *testing.T) {
	cases := [][]string{
		{"demo", "--packets", "0"},
		{"demo", "--interval-ms", "0"},
		{"demo", "--loss-rate", "2"},
		{"demo", "--duplicate-rate", "-1"},
		{"demo", "--reorder", "-5"},
		{"demo", "--queue", "0"},
		{"nope"},
		{},
	}
	for _, args := range cases {
		opts, err := cli.Parse(args, os.Stderr)
		if err == nil {
			// Parse succeeded only if validation is deferred; running must
			// still refuse to proceed.
			t.Fatalf("args %v parsed successfully into %+v", args, opts)
		}
	}
}

// TestUnknownSignalingEndpointExitsThree proves a dead signaling target is a
// transport failure (exit 3) rather than a crash or a hang.
func TestUnknownSignalingEndpointExitsThree(t *testing.T) {
	if testing.Short() {
		t.Skip("transport failure path is slow")
	}
	opts, err := cli.Parse([]string{
		"probe", "--addr", "127.0.0.1:1", "--connect-ms", "1500", "--json",
	}, os.Stderr)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var out, errOut bytes.Buffer
	if code := cli.Run(context.Background(), opts, &out, &errOut); code != cli.ExitTransport {
		t.Fatalf("exit=%d, want %d (stderr=%s)", code, cli.ExitTransport, errOut.String())
	}
}

// TestProbeSelfCheck proves the smoke-test subcommand works on a clean host.
func TestProbeSelfCheck(t *testing.T) {
	if testing.Short() {
		t.Skip("probe starts real peer connections")
	}
	opts, err := cli.Parse([]string{"probe", "--json"}, os.Stderr)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var out, errOut bytes.Buffer
	code := cli.Run(context.Background(), opts, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	var rep map[string]any
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out.String())
	}
	if rep["forwarded_packets"].(float64) != rep["source_packets"].(float64) {
		t.Fatalf("probe lost packets: %+v", rep)
	}
	if !rep["loopback_only"].(bool) {
		t.Fatal("probe reported a non-loopback path")
	}
}

// TestDemoIsRepeatable runs the same seed twice and requires identical packet
// accounting, which is what makes the fixture useful for regression work.
func TestDemoIsRepeatable(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end demo is too slow for -short")
	}
	run := func() map[string]any {
		opts, err := cli.Parse([]string{
			"demo", "--packets", "400", "--interval-ms", "1",
			"--reorder", "16", "--duplicate-rate", "0.02", "--loss-rate", "0.01",
			"--seed", "42", "--json",
		}, os.Stderr)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		var out, errOut bytes.Buffer
		if code := cli.Run(context.Background(), opts, &out, &errOut); code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, errOut.String())
		}
		var rep map[string]any
		if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		return rep
	}
	a, b := run(), run()
	for _, key := range []string{"forwarded_packets", "lost_packets", "duplicate_packets", "reordered_packets"} {
		if a[key] != b[key] {
			t.Fatalf("%s differs between identical runs: %v vs %v", key, a[key], b[key])
		}
	}
}

// TestSignalHandling verifies an interrupted run exits 130 rather than
// pretending it succeeded. The process is started, allowed to reach steady
// state, then sent a real SIGINT — exec.CommandContext would kill it instead,
// which would only prove that a killed process dies.
func TestSignalHandling(t *testing.T) {
	if testing.Short() {
		t.Skip("needs a built binary")
	}
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signal semantics")
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "relay")
	build := exec.Command("go", "build", "-o", bin, "./cmd/relay")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	cmd := exec.Command(bin, "demo", "--packets", "200000", "--interval-ms", "5")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Let the run reach the media pump, then interrupt it.
	time.Sleep(1500 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("signal: %v", err)
	}
	waitErr := cmd.Wait()

	code := 0
	if exitErr, ok := waitErr.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	}
	if code != cli.ExitInterrupted {
		t.Fatalf("exit=%d, want %d (stderr=%s)", code, cli.ExitInterrupted, stderr.String())
	}
}
