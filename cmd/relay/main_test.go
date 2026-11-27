package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestExitCodes 验证 0/2/3/4 退出码约定在 CLI 层的映射。
func TestExitCodes(t *testing.T) {
	base := []string{"selftest", "--seed", "1", "--duration", "2s"}

	t.Run("ok", func(t *testing.T) {
		work := filepath.Join(t.TempDir(), "w")
		code := run(append(base, "--work", work))
		if code != 0 {
			t.Fatalf("exit=%d want 0", code)
		}
	})

	t.Run("bad args exit 3", func(t *testing.T) {
		// 缺少 --work。
		if code := run([]string{"selftest"}); code != 3 {
			t.Fatalf("exit=%d want 3", code)
		}
		// 未知子命令。
		if code := run([]string{"frobnicate"}); code != 3 {
			t.Fatalf("exit=%d want 3", code)
		}
	})

	t.Run("io failure exit 4", func(t *testing.T) {
		// 工作目录指向一个已存在的普通文件，MkdirAll 必然失败。
		blocker := filepath.Join(t.TempDir(), "afile")
		if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		work := filepath.Join(blocker, "w") // blocker 是文件，无法在其下建目录
		code := run(append(base, "--work", work))
		if code != 4 {
			t.Fatalf("exit=%d want 4", code)
		}
	})
}
