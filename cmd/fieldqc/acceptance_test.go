package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"fieldqc/internal/fault"
	"fieldqc/internal/report"
	"fieldqc/internal/stream"
)

func testConfig() config {
	return config{seed: 41, hours: 1, tracks: 4, loss: 0.03, window: 10 * time.Second}
}

// TestRunAllExitsOK runs the documented acceptance command shape and
// requires exit 0 (all samples handled as expected).
func TestRunAllExitsOK(t *testing.T) {
	code := run([]string{
		"demo", "--case", "all", "--seed", "41", "--hours", "1",
		"--tracks", "4", "--loss", "0.03", "--window", "10s",
	})
	if code != exitOK {
		t.Fatalf("demo --case all exited %d, want %d", code, exitOK)
	}
}

// TestExitCodes locks the 0/1/2/3 contract.
func TestExitCodes(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"no command", nil, exitInvalid},
		{"unknown command", []string{"nope"}, exitInvalid},
		{"unknown case", []string{"demo", "--case", "nope"}, exitInvalid},
		{"odd tracks", []string{"demo", "--tracks", "3"}, exitInvalid},
		{"loss too high", []string{"demo", "--loss", "1.5"}, exitInvalid},
		{"zero hours", []string{"demo", "--hours", "0"}, exitInvalid},
		{"clean case", []string{"demo", "--case", "normal", "--hours", "1"}, exitOK},
		{"located damage", []string{"demo", "--case", "truncated-box", "--hours", "1"}, exitDamaged},
		{"cancel", []string{"cancel"}, exitAborted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := run(tc.args); got != tc.want {
				t.Fatalf("run(%v) = %d, want %d", tc.args, got, tc.want)
			}
		})
	}
}

// TestSyncWithinTargetAndDigestStable is the regression for the window
// tail bias that previously reported sync P95 = 40 ms.
func TestSyncWithinTargetAndDigestStable(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig()

	normal, err := runCase(ctx, cfg, "normal")
	if err != nil {
		t.Fatal(err)
	}
	if normal.SyncP95Ms > 20 {
		t.Fatalf("sync_p95_ms = %v, want <= 20", normal.SyncP95Ms)
	}
	if normal.DamagedSpans != 0 {
		t.Fatalf("normal damaged_spans = %d, want 0", normal.DamagedSpans)
	}
	again, err := runCase(ctx, cfg, "normal")
	if err != nil {
		t.Fatal(err)
	}
	if normal.Digest != again.Digest {
		t.Fatalf("digest not repeatable: %s vs %s", normal.Digest, again.Digest)
	}
}

// TestAllDamagedCasesLocateSpans checks localization, recovery and the
// per-window processing budget on the exact acceptance parameters.
func TestAllDamagedCasesLocateSpans(t *testing.T) {
	ctx := context.Background()
	cfg := config{seed: 41, hours: 2, tracks: 4, loss: 0.03, window: 10 * time.Second}

	for _, name := range allCases {
		line, err := runCase(ctx, cfg, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		wantDamage := name != "normal"
		if (line.DamagedSpans > 0) != wantDamage {
			t.Fatalf("%s: damaged_spans=%d, want damage=%v", name, line.DamagedSpans, wantDamage)
		}
		if line.SyncP95Ms > 20 {
			t.Fatalf("%s: sync_p95_ms=%v > 20", name, line.SyncP95Ms)
		}
		if line.P95WindowMs > 250 {
			t.Fatalf("%s: p95_window_ms=%v > 250", name, line.P95WindowMs)
		}
	}

	burst, err := runCase(ctx, cfg, "rtp-burst")
	if err != nil {
		t.Fatal(err)
	}
	if burst.LossRecoveryPct < 70 {
		t.Fatalf("loss_recovery_pct = %v, want >= 70", burst.LossRecoveryPct)
	}

	// report.P95 empty guard.
	if got := report.P95(nil); got != 0 {
		t.Fatalf("P95(nil) = %v, want 0", got)
	}
}

// TestRunWindowsCanceled maps a canceled context to ErrAborted.
func TestRunWindowsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _, err := runWindows(ctx, testConfig(), "normal", fault.NewModel())
	if !errors.Is(err, stream.ErrAborted) {
		t.Fatalf("err = %v, want stream.ErrAborted", err)
	}
}
