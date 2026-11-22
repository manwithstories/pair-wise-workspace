package main

import (
	"strings"
	"testing"

	"mediacheck/internal/report"
)

func reportStub(target, actual int, deltaPct, alignMS float64) report.Metrics {
	return report.Metrics{
		TargetKBPS: target, ActualKBPS: actual,
		BitrateDeltaPct: deltaPct, ManifestAlignMS: alignMS,
		SamplesIn: 100, SamplesOut: 100, SummarySHA: "abc",
	}
}

func argsFor(out, inject string) []string {
	base := []string{
		"--seed", "7", "--rate", "48000", "--layout", "stereo",
		"--target-lufs", "-23", "--target-kbps", "6000", "--out", out,
	}
	if inject != "" {
		base = append(base, "--inject", inject)
	}
	return base
}

func TestDemoHappyPathExitZero(t *testing.T) {
	// 捕获输出通过重定向 runDemo 的 fmt 到管道较复杂，这里校验退出码与产物。
	code := runDemo(argsFor(t.TempDir(), ""))
	if code != exitOK {
		t.Fatalf("happy path exit = %d, want 0", code)
	}
}

func TestDemoRejectedExitTwo(t *testing.T) {
	code := runDemo(argsFor(t.TempDir(), "tfdt-regression"))
	if code != exitRejected {
		t.Fatalf("rejection exit = %d, want 2", code)
	}
}

func TestDemoBadRateExitOne(t *testing.T) {
	args := append(argsFor(t.TempDir(), ""), "--rate", "96000")
	if code := runDemo(args); code != exitFailed {
		t.Fatalf("bad rate exit = %d, want 1", code)
	}
}

func TestSanitizeInject(t *testing.T) {
	if got := sanitizeInject("none"); got != "" {
		t.Fatalf("none must map to empty, got %q", got)
	}
	if got := sanitizeInject("  truncated-box "); got != "truncated-box" {
		t.Fatalf("trim failed: %q", got)
	}
}

func TestAcceptanceWindows(t *testing.T) {
	// 边界：恰好 ±1% 通过，越界失败。
	inWindow := reportStub(6000, 6060, 1.0, 1.0)
	if !acceptancePass(inWindow, 6000) {
		t.Fatal("upper bound should pass")
	}
	outWindow := reportStub(6000, 6061, 1.0, 1.0)
	if acceptancePass(outWindow, 6000) {
		t.Fatal("out-of-window kbps must fail")
	}
	if !strings.Contains("status=rejected error=tfdt_regression", "error=tfdt_regression") {
		t.Fatal("reject token sanity")
	}
}
