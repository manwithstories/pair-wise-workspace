package main

import (
	"os"
	"testing"
)

// osWriteFile 是 os.WriteFile 的测试内别名，集中文件写入入口。
func osWriteFile(path string, b []byte) error {
	return os.WriteFile(path, b, 0o644)
}

// mustOpen 打开测试输入文件，失败即终止。
func mustOpen(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}
