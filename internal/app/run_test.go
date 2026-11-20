package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mediagraph/internal/pipeline"
)

func testConfig(dir string) *pipeline.Config {
	return &pipeline.Config{
		Workdir: dir, Seed: 42, InterruptAfter: -1,
		WindowMs: 4000, MinWindowMs: 1500,
	}
}

// TestDemoFullRun 验证完整运行的摘要指标与确定性。
func TestDemoFullRun(t *testing.T) {
	dir := t.TempDir()
	sum, interrupted, err := Demo(testConfig(dir), -1)
	if err != nil {
		t.Fatalf("demo: %v", err)
	}
	if interrupted {
		t.Fatal("unexpected interrupt")
	}
	if sum.Segments == 0 || sum.RecoveredCues != 5 || sum.Deduplicated != sum.Segments {
		t.Fatalf("bad summary: %+v", sum)
	}
	if sum.FirstErrorOffset < 0 {
		t.Fatal("expected recoverable corruption to be located")
	}
	if len(sum.OutputSHA256) != 64 {
		t.Fatalf("output_sha256 not 64 hex: %q", sum.OutputSHA256)
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
		t.Fatalf("manifest missing: %v", err)
	}
}

// TestInterruptResumeDeterministic 验证中断运行的清单与一次性完整运行逐字节一致。
func TestInterruptResumeDeterministic(t *testing.T) {
	dirA := t.TempDir()
	if _, interrupted, err := Demo(testConfig(dirA), 3); err != nil || !interrupted {
		t.Fatalf("demo interrupt: interrupted=%v err=%v", interrupted, err)
	}
	sumA, interrupted, err := Resume(dirA)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if interrupted {
		t.Fatal("still interrupted after resume")
	}
	dirB := t.TempDir()
	sumB, _, err := Demo(testConfig(dirB), -1)
	if err != nil {
		t.Fatalf("demo full: %v", err)
	}
	if sumA != sumB {
		t.Fatalf("summary mismatch:\nresumed %+v\nfull    %+v", sumA, sumB)
	}
	for _, name := range []string{"manifest.json", "provenance.json", "checkpoint.json"} {
		a, err := os.ReadFile(filepath.Join(dirA, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		b, err := os.ReadFile(filepath.Join(dirB, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(a) != string(b) {
			t.Fatalf("%s differs between resumed and full run", name)
		}
	}
}

// TestResumeIdempotent 验证重复提交同一任务输出一致。
func TestResumeIdempotent(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := Demo(testConfig(dir), -1); err != nil {
		t.Fatalf("demo: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Resume(dir); err != nil {
		t.Fatalf("resume: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("manifest changed after idempotent resume")
	}
}

// TestResumeRejectsChangedInput 验证输入变更拒绝复用旧检查点。
func TestResumeRejectsChangedInput(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := Demo(testConfig(dir), -1); err != nil {
		t.Fatalf("demo: %v", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "good.srt"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("tamper\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, _, err := Resume(dir); !errors.Is(err, ErrCheckpoint) {
		t.Fatalf("expected ErrCheckpoint, got %v", err)
	}
}

// TestResumeWithoutCheckpoint 验证无检查点时续跑失败。
func TestResumeWithoutCheckpoint(t *testing.T) {
	if _, _, err := Resume(t.TempDir()); !errors.Is(err, ErrCheckpoint) {
		t.Fatalf("expected ErrCheckpoint, got %v", err)
	}
}

// TestVerifyFixtures 验证损坏夹具的首个错误定位与恢复边界，及正常夹具无问题。
func TestVerifyFixtures(t *testing.T) {
	bad, err := Verify("corrupt", 42, "")
	if err != nil {
		t.Fatalf("verify corrupt: %v", err)
	}
	if bad.OK {
		t.Fatal("corrupt fixture reported OK")
	}
	if bad.FirstErrorOffset != 43 || bad.FirstErrorFile != "corrupt.srt" {
		t.Fatalf("first error = %s:%d, want corrupt.srt:43", bad.FirstErrorFile, bad.FirstErrorOffset)
	}
	if bad.RecoveredCues != 5 {
		t.Fatalf("recovered_cues = %d, want 5", bad.RecoveredCues)
	}
	good, err := Verify("good", 42, "")
	if err != nil {
		t.Fatalf("verify good: %v", err)
	}
	if !good.OK || good.FirstErrorOffset != -1 {
		t.Fatalf("good fixture: %+v", good)
	}
	if _, err := Verify("bogus", 42, ""); err == nil {
		t.Fatal("expected error for unknown fixture")
	}
}

// TestSeedDeterminism 验证固定种子重复执行校验和相同，不同种子不同。
func TestSeedDeterminism(t *testing.T) {
	run := func(seed int64) Summary {
		cfg := testConfig(t.TempDir())
		cfg.Seed = seed
		sum, _, err := Demo(cfg, -1)
		if err != nil {
			t.Fatalf("demo seed %d: %v", seed, err)
		}
		return sum
	}
	if a, b := run(42), run(42); a.OutputSHA256 != b.OutputSHA256 {
		t.Fatal("same seed produced different output hash")
	}
	if a, b := run(42), run(7); a.OutputSHA256 == b.OutputSHA256 {
		t.Fatal("different seeds produced identical output hash")
	}
}
