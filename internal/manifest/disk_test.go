package manifest

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestWriteAtomicDiskFailureNoLeftover 验证目标目录不可写时原子写失败，
// 且不会留下半成品清单（对应磁盘故障后可安全恢复的约束）。
func TestWriteAtomicDiskFailureNoLeftover(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("权限模型差异，跳过 Windows")
	}
	parent := t.TempDir()
	readOnly := filepath.Join(parent, "readonly")
	if err := os.MkdirAll(readOnly, 0o555); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(readOnly, 0o755) }()

	m := Build(sampleInput())
	err := WriteAtomic(filepath.Join(readOnly, "nested", "manifest.json"), m)
	if err == nil {
		t.Fatalf("只读目录下原子写应当失败")
	}
	// 失败后不得残留任何临时文件。
	matches, _ := filepath.Glob(filepath.Join(readOnly, ".manifest-*.tmp"))
	if len(matches) != 0 {
		t.Fatalf("磁盘失败后残留临时文件: %v", matches)
	}
}

// TestWriteAtomicReplacesAtomically 验证重名清单被整体替换而非追加。
func TestWriteAtomicReplacesAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	m1 := Build(sampleInput())
	if err := WriteAtomic(path, m1); err != nil {
		t.Fatal(err)
	}
	in2 := sampleInput()
	in2.CutPoints = in2.CutPoints[:1] // 更少分片
	m2 := Build(in2)
	if err := WriteAtomic(path, m2); err != nil {
		t.Fatal(err)
	}
	got, err := ReadAtomic(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.SegmentCount != m2.SegmentCount || got.SHA256 != m2.SHA256 {
		t.Fatalf("原子替换后内容不是最新版本")
	}
}
