package index

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T, in, cfg string) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir(), 4, in, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestWriteAndReadRoundtrip(t *testing.T) {
	s := newTestStore(t, "in", "cfg")
	cp, err := s.Save(0, 4, []int{0, 1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadValid(checkpointPath(s.Dir(), 0))
	if err != nil {
		t.Fatalf("ReadValid: %v", err)
	}
	if got.NextSeq != 4 || len(got.DoneSeqs) != 4 || got.OutputSHA != cp.OutputSHA {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
}

func TestCorruptChecksumFallsBack(t *testing.T) {
	s := newTestStore(t, "in", "cfg")
	if _, err := s.Save(0, 4, []int{0, 1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	s.AppendOutput([]byte("block-1"))
	if _, err := s.Save(1, 8, []int{4, 5, 6, 7}); err != nil {
		t.Fatal(err)
	}
	// 破坏最新块（截断 + 篡改字节）。
	p1 := checkpointPath(s.Dir(), 1)
	data, _ := os.ReadFile(p1)
	if err := os.WriteFile(p1, data[:len(data)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	cp, corrupt, ok := s.LatestValid(1)
	if !ok {
		t.Fatal("expected fallback to chunk 0")
	}
	if len(corrupt) != 1 || corrupt[0] != 1 {
		t.Fatalf("corrupt list = %v", corrupt)
	}
	if cp.NextSeq != 4 {
		t.Fatalf("fell back to wrong checkpoint: next=%d", cp.NextSeq)
	}
}

func TestRejectResumeOnInputChange(t *testing.T) {
	dir := t.TempDir()
	s1, err := NewStore(dir, 4, "input-A", "cfg")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s1.Save(0, 4, []int{0, 1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	// 输入摘要变化（模拟换了输入目录），配置不变。
	s2, err := NewStore(dir, 4, "input-B", "cfg")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s2.Resume(0)
	if !errors.Is(err, ErrRejectResume) {
		t.Fatalf("err = %v, want ErrRejectResume", err)
	}
}

func TestResumeIdempotentOutputSHA(t *testing.T) {
	dir := t.TempDir()
	// 首次运行：写入两个块后得到最终 SHA。
	s1, _ := NewStore(dir, 2, "in", "cfg")
	s1.AppendOutput([]byte("block-0"))
	cp0, _ := s1.Save(0, 2, []int{0, 1})
	s1.AppendOutput([]byte("block-1"))
	wantSHA := s1.OutputSHA()
	if _, err := s1.Save(1, 4, []int{2, 3}); err != nil {
		t.Fatal(err)
	}
	// 模拟中断后从块 0 检查点恢复，重放块 1，最终 SHA 必须一致。
	s2, err := NewStore(dir, 2, "in", "cfg")
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := s2.Resume(0)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.NextSeq != cp0.NextSeq {
		t.Fatalf("resumed next = %d", resumed.NextSeq)
	}
	s2.AppendOutput([]byte("block-1"))
	if got := s2.OutputSHA(); got != wantSHA {
		t.Fatalf("resumed SHA %s != first run %s", got, wantSHA)
	}
}

func TestNoUsableCheckpoint(t *testing.T) {
	s := newTestStore(t, "in", "cfg")
	if _, err := s.Resume(3); !errors.Is(err, ErrCheckpointCorrupt) {
		t.Fatalf("err = %v, want ErrCheckpointCorrupt", err)
	}
}

func TestDigestInputsStableAndSensitive(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}
	d1, err := DigestInputs(dir)
	if err != nil {
		t.Fatal(err)
	}
	d2, _ := DigestInputs(dir)
	if d1 != d2 {
		t.Fatal("input digest must be deterministic")
	}
	// 改动任一文件内容，摘要必须变化。
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("HELLO"), 0o644); err != nil {
		t.Fatal(err)
	}
	d3, _ := DigestInputs(dir)
	if d3 == d1 {
		t.Fatal("input digest must change when content changes")
	}
}

func TestDigestConfigSensitive(t *testing.T) {
	a := DigestConfig(4, 2, 1<<24, "")
	b := DigestConfig(8, 2, 1<<24, "")
	c := DigestConfig(4, 2, 1<<24, "edge")
	if a == b || a == c {
		t.Fatal("config digest must distinguish parameters")
	}
}
