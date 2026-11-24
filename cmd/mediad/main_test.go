package main

import (
	"os"
	"path/filepath"
	"testing"

	"media-pipeline/internal/engine"
	"media-pipeline/internal/fault"
	"media-pipeline/internal/resume"
)

// TestCheckpointConflictExitCode 验证检查点与当前输入不匹配时，
// 错误类别映射为不可恢复冲突（退出码 3）。
func TestCheckpointConflictExitCode(t *testing.T) {
	dir := t.TempDir()
	cfg := engine.DefaultConfig()

	// 第一次运行写出检查点。
	if _, err := engine.Run(42, dir, cfg, true); err != nil {
		t.Fatal(err)
	}
	cp, err := resume.Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cp == nil {
		t.Fatal("expected checkpoint after resume run")
	}

	// 篡改参数摘要：模拟以不同配置/输入续跑。
	cp.ParamDigest = "tampered-digest"
	if err := resume.Write(filepath.Join(dir, "tampered"), *cp); err != nil {
		t.Fatal(err)
	}
	loaded, err := resume.Read(filepath.Join(dir, "tampered"))
	if err != nil {
		t.Fatal(err)
	}
	sources := cp.Sources
	_, verifyErr := resume.Verify(loaded, "different-params", sources)
	if verifyErr == nil {
		t.Fatal("expected checkpoint conflict")
	}
	if got := fault.ExitCode(verifyErr); got != fault.ExitCheckpointFault {
		t.Fatalf("exit code = %d, want %d", got, fault.ExitCheckpointFault)
	}
}

// TestRunRejectsUnknownOutput 保证输出目录校验对空/非法路径报错。
func TestRunRejectsUnknownOutput(t *testing.T) {
	if err := ensureLocalDir(""); err == nil {
		t.Fatal("empty output dir should be rejected")
	}
	good := filepath.Join(t.TempDir(), "out")
	if err := ensureLocalDir(good); err != nil {
		t.Fatalf("valid dir rejected: %v", err)
	}
	if _, err := os.Stat(good); err != nil {
		t.Fatalf("output dir not created: %v", err)
	}
}
