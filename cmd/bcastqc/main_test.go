package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDemoExitCodesAndRejection(t *testing.T) {
	outDir := filepath.Join(t.TempDir(), "out")

	// 缺 --out-dir：参数错误，退出码 2。
	if code := run([]string{"demo", "--seed", "42"}); code != exitArgError {
		t.Fatalf("missing out-dir exit=%d want %d", code, exitArgError)
	}

	// 完整 demo，含 truncate 预期拒绝：退出码 0。
	code := run([]string{
		"demo",
		"--suite", "clean,recovery,truncate,overflow,stall",
		"--seed", "42",
		"--queue-bytes", "4194304",
		"--out-dir", outDir,
	})
	if code != exitOK {
		t.Fatalf("demo exit=%d want %d", code, exitOK)
	}

	// truncate 不得产出 webm，其余四个健康场景产出。
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	webm := map[string]bool{}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".webm" {
			webm[e.Name()] = true
		}
	}
	if webm["truncate.webm"] {
		t.Fatal("truncated sample must not publish a half-finished file")
	}
	for _, healthy := range []string{"clean.webm", "recovery.webm", "overflow.webm", "stall.webm"} {
		if !webm[healthy] {
			t.Fatalf("expected output %s missing", healthy)
		}
	}
	if _, err := os.Stat(filepath.Join(outDir, "manifest.json")); err != nil {
		t.Fatalf("manifest missing: %v", err)
	}
}

func TestVerifyIndependent(t *testing.T) {
	outDir := filepath.Join(t.TempDir(), "out")
	if code := run([]string{"demo", "--suite", "clean", "--seed", "42",
		"--queue-bytes", "4194304", "--out-dir", outDir}); code != exitOK {
		t.Fatalf("demo setup exit=%d", code)
	}
	if code := run([]string{"verify", "--dir", outDir}); code != exitOK {
		t.Fatalf("verify exit=%d want %d", code, exitOK)
	}

	// 空目录：输入错误，退出码 2。
	empty := t.TempDir()
	if code := run([]string{"verify", "--dir", empty}); code != exitArgError {
		t.Fatalf("empty verify exit=%d want %d", code, exitArgError)
	}

	// 不存在目录：I/O 错误，退出码 3。
	if code := run([]string{"verify", "--dir", filepath.Join(t.TempDir(), "nope")}); code != exitIOError {
		t.Fatalf("missing dir exit=%d want %d", code, exitIOError)
	}
}

func TestUnknownCommand(t *testing.T) {
	if code := run([]string{"frobnicate"}); code != exitArgError {
		t.Fatalf("unknown cmd exit=%d want %d", code, exitArgError)
	}
}
