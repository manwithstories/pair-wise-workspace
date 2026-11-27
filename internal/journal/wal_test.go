package journal

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"review/internal/fault"
)

func writeFile(dir, name, content string) (string, error) {
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		return "", err
	}
	return HashDir(dir)
}

func TestCommitFlow(t *testing.T) {
	dir := t.TempDir()
	wal, err := Open(filepath.Join(dir, "wal.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer wal.Close()
	tx := NewTx(wal, filepath.Join(dir, "out"))
	d1, err := tx.Run("parse", "in-1", func(d string) (string, error) {
		return writeFile(d, "result.txt", "parsed")
	})
	if err != nil {
		t.Fatal(err)
	}
	if tx.Commits["parse"] != 1 {
		t.Fatal("commit not counted")
	}
	// 产物已原子改名到位
	if _, err := os.Stat(filepath.Join(dir, "out", "parse", "result.txt")); err != nil {
		t.Fatal("final output missing")
	}
	// 重放得到已提交边界
	committed, dangling, err := Replay(filepath.Join(dir, "wal.log"))
	if err != nil {
		t.Fatal(err)
	}
	if dangling != 0 || len(committed) != 1 || committed[0].Digest != d1 {
		t.Fatalf("committed=%v dangling=%d", committed, dangling)
	}
}

func TestAbortRollback(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal.log")
	wal, _ := Open(walPath)
	tx := NewTx(wal, filepath.Join(dir, "out"))
	// 先成功一个阶段
	if _, err := tx.Run("parse", "in", func(d string) (string, error) {
		return writeFile(d, "ok.txt", "v1")
	}); err != nil {
		t.Fatal(err)
	}
	// 第二阶段中途失败（模拟提交中断）
	_, err := tx.Run("render", "in", func(d string) (string, error) {
		os.WriteFile(filepath.Join(d, "partial.jpg"), []byte("x"), 0o644)
		return "", errors.New("simulated crash")
	})
	if err == nil {
		t.Fatal("want stage failure")
	}
	var fe *fault.Error
	if !errors.As(err, &fe) || fe.Kind != fault.KindTx {
		t.Fatalf("want tx fault, got %v", err)
	}
	// 临时产物已清理，最终目录不存在 → 回滚到最后有效边界
	if _, err := os.Stat(filepath.Join(dir, "out", ".tmp-render")); !os.IsNotExist(err) {
		t.Fatal("tmp dir not cleaned")
	}
	if _, err := os.Stat(filepath.Join(dir, "out", "render")); !os.IsNotExist(err) {
		t.Fatal("aborted stage must not have final output")
	}
	committed, dangling, _ := Replay(walPath)
	if dangling != 0 || len(committed) != 1 || committed[0].Stage != "parse" {
		t.Fatalf("committed=%v dangling=%d", committed, dangling)
	}
	wal.Close()
}

func TestReplayHashConsistency(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal.log")
	run := func() map[string]string {
		wal, _ := Open(walPath)
		defer wal.Close()
		tx := NewTx(wal, filepath.Join(dir, "out"))
		digests := map[string]string{}
		for _, stage := range []string{"parse", "mix"} {
			d, err := tx.Run(stage, "in", func(dd string) (string, error) {
				return writeFile(dd, stage+".txt", "content-"+stage)
			})
			if err != nil {
				t.Fatal(err)
			}
			digests[stage] = d
		}
		return digests
	}
	first := run()
	// 重放校验：摘要一致
	ok, err := Verify(walPath, first)
	if err != nil || !ok {
		t.Fatalf("verify=%v err=%v", ok, err)
	}
	// 摘要不符应检出
	bad := map[string]string{"parse": "wrong"}
	if ok, _ := Verify(walPath, bad); ok {
		t.Fatal("tampered digest must fail verify")
	}
}

func TestHashDirDeterministic(t *testing.T) {
	dir := t.TempDir()
	writeFile(dir, "a.txt", "1")
	writeFile(dir, "b.txt", "2")
	h1, _ := HashDir(dir)
	h2, _ := HashDir(dir)
	if h1 != h2 || len(h1) != 64 {
		t.Fatalf("hash=%q", h1)
	}
}
