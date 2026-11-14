package main

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"relayctl/internal/fixture"
	"relayctl/internal/peer"
	"relayctl/internal/relay"
	"relayctl/internal/signal"
)

func TestShortDemoAndVerify(t *testing.T) {
	impair := fixture.Impairment{Loss: 0.02, Duplicate: 0.02, Reorder: 0.03}
	generated, err := fixture.Generate(fixture.Options{
		Packets: 600, Duration: 1 * time.Second, Seed: 7, Impairment: impair,
	})
	if err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(t.TempDir(), "report.json")
	report, err := executeDemo(context.Background(), demoConfig{
		packets: 600, duration: time.Second, seed: 7, impair: impair,
		sink: "127.0.0.1:0", bind: "127.0.0.1:0", jsonPath: reportPath,
		ssrc: 0x524C5941, startSeq: 0x4000,
	}, generated, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if report.Emitted != 600 || report.Forwarded != report.Unique || report.Unique == 0 {
		t.Fatalf("metrics = %+v", report.Metrics)
	}
	if report.Received < 560 || report.Received > 600 {
		t.Fatalf("received = %d", report.Received)
	}
	if report.Duplicates == 0 || report.Reordered == 0 {
		t.Fatalf("impairment counts missing: duplicates=%d reordered=%d", report.Duplicates, report.Reordered)
	}
	if report.JitterMS.P50 > 2 || report.JitterMS.P95 > 30 || report.JitterMS.Max > 60 {
		t.Fatalf("jitter = %+v", report.JitterMS)
	}
	if report.SinkPackets+2 < report.Forwarded {
		t.Fatalf("sink packets=%d forwarded=%d", report.SinkPackets, report.Forwarded)
	}
	if report.ForwardBytes == 0 || report.SelectedPair == "" || report.BannedCandidates != 0 {
		t.Fatalf("report = %+v", report)
	}
	if _, err := relay.ReadReport(reportPath); err != nil {
		t.Fatal(err)
	}
	if err := runVerify([]string{"--report", reportPath, "--min-forward-ratio", "0.95", "--max-jitter-p95-ms", "30"}, io.Discard); err != nil {
		t.Fatalf("verify should pass: %v", err)
	}
	err = runVerify([]string{"--report", reportPath, "--min-forward-ratio", "0.99", "--max-jitter-p95-ms", "0"}, io.Discard)
	var threshold *thresholdError
	if !errors.As(err, &threshold) {
		t.Fatalf("verify error = %v", err)
	}
}

func TestLoopbackArgumentPolicy(t *testing.T) {
	if err := validateLoopbackAddr("127.0.0.1:1"); err != nil {
		t.Fatal(err)
	}
	if err := validateLoopbackAddr("0.0.0.0:1"); err == nil {
		t.Fatal("expected non-loopback bind rejection")
	}
}

func TestOccupiedSignalPortErrorCode(t *testing.T) {
	occupied, err := signal.Listen("127.0.0.1:0", signal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	err = runDemo([]string{
		"--packets", "10", "--duration", "10ms", "--seed", "1",
		"--sink", "127.0.0.1:0", "--bind", occupied.Addr().String(),
	}, io.Discard)
	code, errorCode, hint := classifyError(err)
	if code != exitRuntime || errorCode != "port_in_use" || hint == "" {
		t.Fatalf("code=%d error_code=%s hint=%q err=%v", code, errorCode, hint, err)
	}
}

func TestSignalAndICEErrorCodes(t *testing.T) {
	code, errorCode, _ := classifyError(signal.ErrSignalTimeout)
	if code != exitRuntime || errorCode != "signal_timeout" {
		t.Fatalf("signal timeout mapped to %d/%s", code, errorCode)
	}
	code, errorCode, _ = classifyError(peer.ErrICEFailed)
	if code != exitRuntime || errorCode != "ice_failed" {
		t.Fatalf("ICE failure mapped to %d/%s", code, errorCode)
	}
}
