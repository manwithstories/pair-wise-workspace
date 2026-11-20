package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunDemoExitCodes(t *testing.T) {
	dir := t.TempDir()

	// 损坏注入 => 退出码 2。
	code := run([]string{
		"demo", "--codec", "av1", "--representations", "3",
		"--frames", "1800", "--block-frames", "600",
		"--inject-corruption-at", "900", "--resume",
		"--work-dir", filepath.Join(dir, "corrupt"),
	})
	if code != exitCorruption {
		t.Fatalf("损坏场景退出码=%d, 期望 2", code)
	}

	// 无损坏 => 退出码 0。
	code = run([]string{
		"demo", "--codec", "h264", "--representations", "2",
		"--frames", "900", "--block-frames", "300",
		"--inject-corruption-at", "-1",
		"--work-dir", filepath.Join(dir, "clean"),
	})
	if code != exitOK {
		t.Fatalf("干净场景退出码=%d, 期望 0", code)
	}
}

func TestRunDemoBadArgs(t *testing.T) {
	if code := run([]string{"demo", "--codec", "bogus"}); code != exitConfigOrIOErr {
		t.Fatalf("坏编码退出码=%d, 期望 4", code)
	}
	if code := run([]string{"demo", "--frames", "0"}); code != exitConfigOrIOErr {
		t.Fatalf("非法帧数退出码=%d, 期望 4", code)
	}
	if code := run([]string{"unknown"}); code != exitConfigOrIOErr {
		t.Fatalf("未知子命令退出码=%d, 期望 4", code)
	}
}

func TestVersionAndHelp(t *testing.T) {
	if code := run([]string{"version"}); code != exitOK {
		t.Fatalf("version 退出码=%d", code)
	}
	if code := run([]string{"help"}); code != exitOK {
		t.Fatalf("help 退出码=%d", code)
	}
}

func TestAuditSubcommand(t *testing.T) {
	dir := t.TempDir()
	code := run([]string{
		"audit", "--work-dir", dir, "--codec", "av1",
		"--representations", "2",
	})
	if code != exitOK {
		t.Fatalf("audit 退出码=%d, 期望 0", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
		t.Fatalf("audit 未写出清单: %v", err)
	}
}

func TestAuditRequiresWorkDir(t *testing.T) {
	if code := run([]string{"audit"}); code != exitConfigOrIOErr {
		t.Fatalf("缺少 work-dir 应退出 4, got %d", code)
	}
}
