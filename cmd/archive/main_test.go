package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"archive/internal/report"
)

// run() must honour the documented exit-code contract: 0 pass, 2 bad
// parameters, 3 asset failures, 4 verification failure.
func TestExitCodeParameterErrors(t *testing.T) {
	cases := [][]string{
		nil,                       // no subcommand
		{"bogus"},                 // unknown subcommand
		{"demo", "--chunk-bytes", "0"},
		{"demo", "--burst-loss", "-1"},
		{"demo", "--reorder", "-2"},
		{"demo", "--bandwidth-mbps", "1,0,2"},
		{"demo", "--bandwidth-mbps", "1,x"},
		{"demo", "--bandwidth-mbps", ""},
		{"demo", "--no-such-flag"},
	}
	for _, args := range cases {
		if code := run(context.Background(), args); code != exitParamError {
			t.Fatalf("args %v: exit %d, want %d", args, code, exitParamError)
		}
	}
}

// The acceptance invocation must pass and report every required field.
func TestAcceptanceDemo(t *testing.T) {
	out := t.TempDir()
	code, sum := runDemo(context.Background(), demoConfig{
		seed: 41, burstLoss: 7, reorder: 12,
		bandwidth: []float64{1, 8, 2}, chunkBytes: 1 << 20, outDir: out,
	})
	if code != exitOK {
		t.Fatalf("exit %d, assets_failed=%d", code, sum.AssetsFailed)
	}
	if sum.AVSyncErrorMs.P95 > maxAVSyncP95Ms {
		t.Fatalf("av sync p95 %.2f > %.2f", sum.AVSyncErrorMs.P95, maxAVSyncP95Ms)
	}
	if sum.PacketRecoveryRate < minRecoveryRate {
		t.Fatalf("recovery %.4f < %.2f", sum.PacketRecoveryRate, minRecoveryRate)
	}
	if sum.ProcessingP95Ms > maxProcessingP95 {
		t.Fatalf("processing p95 %.1f > %.0f", sum.ProcessingP95Ms, maxProcessingP95)
	}
	if !sum.CheckpointResumeVerified {
		t.Fatal("checkpoint resume not verified")
	}
	if sum.CorruptCasesDetected < 4 || sum.CorruptCasesDetected != sum.CorruptCasesTotal {
		t.Fatalf("corrupt %d/%d", sum.CorruptCasesDetected, sum.CorruptCasesTotal)
	}
	// every good asset produced a non-empty output file
	for _, a := range sum.Assets {
		if a.Status != "ok" {
			continue
		}
		p := filepath.Join(out, "out", a.Name+".mp4")
		if st, err := os.Stat(p); err != nil || st.Size() == 0 {
			t.Fatalf("%s: output missing or empty (%v)", a.Name, err)
		}
	}
	// the JSON document carries the documented field names
	var buf bytes.Buffer
	if err := report.WriteJSON(&buf, sum); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		`"assets"`, `"av_sync_error_ms"`, `"packet_recovery_rate"`,
		`"processing_p95_ms"`, `"checkpoint_resume_verified"`, `"corrupt_cases_detected"`,
	} {
		if !bytes.Contains(buf.Bytes(), []byte(field)) {
			t.Fatalf("report missing %s", field)
		}
	}
}

// Determinism: the same seed must produce byte-identical outputs.
func TestDemoDeterministic(t *testing.T) {
	cfg := demoConfig{seed: 7, burstLoss: 3, reorder: 12, bandwidth: []float64{1, 8, 2}, chunkBytes: 1 << 20}
	var hashes [2][]byte
	for i := range hashes {
		dir := t.TempDir()
		c := cfg
		c.outDir = dir
		if code, _ := runDemo(context.Background(), c); code != exitOK {
			t.Fatalf("run %d exited %d", i, code)
		}
		data, err := os.ReadFile(filepath.Join(dir, "out", "big-dash.mp4"))
		if err != nil {
			t.Fatal(err)
		}
		hashes[i] = data
	}
	if !bytes.Equal(hashes[0], hashes[1]) {
		t.Fatal("same seed produced different output")
	}
}
