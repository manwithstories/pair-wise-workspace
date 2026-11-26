package journal

import (
	"os"
	"path/filepath"
	"testing"

	"archivemedia/internal/model"
)

func sampleRecord(key string) *SegmentRecord {
	return &SegmentRecord{
		SegmentID: "seg-1", SourceID: "tape.arc", IdempotencyKey: key,
		AdapterName: "arc1", AdapterVersion: "1.0.0",
		FilePath: "segments/seg-1.arc", StartUs: 0, EndUs: 100_000,
		Params: map[string]string{"cut": "nearest"},
	}
}

func TestIdempotencyKeyDeterministic(t *testing.T) {
	params := map[string]string{"b": "2", "a": "1"} // 乱序
	k1 := IdempotencyKey("digest123", params, "arc1", "1.0.0")
	k2 := IdempotencyKey("digest123", map[string]string{"a": "1", "b": "2"}, "arc1", "1.0.0")
	if k1 != k2 {
		t.Fatal("参数顺序不得影响幂等键")
	}
	k3 := IdempotencyKey("digest123", params, "arc1", "2.0.0")
	if k1 == k3 {
		t.Fatal("适配器版本变化必须产生不同幂等键")
	}
	if len(k1) != 64 {
		t.Fatalf("幂等键应为 sha256 十六进制, len=%d", len(k1))
	}
}

func TestCommitRequiresFileOnDisk(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	key := "k1"
	if _, _, err := s.Prepare(sampleRecord(key)); err != nil {
		t.Fatal(err)
	}
	// 文件尚未落盘：提交必须失败
	if err := s.Commit(key, "whatever"); model.KindOf(err) != model.KindCheckpoint {
		t.Fatalf("无文件提交应被拦截: %v", err)
	}
	// 落盘后提交成功，且哈希不匹配仍拒绝
	segDir := filepath.Join(root, "segments")
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte("segment-bytes")
	if err := os.WriteFile(filepath.Join(segDir, "seg-1.arc"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	wantHash, _ := HashFile(filepath.Join(segDir, "seg-1.arc"))
	if err := s.Commit(key, "deadbeef"); model.KindOf(err) != model.KindCheckpoint {
		t.Fatalf("哈希不匹配应拒绝: %v", err)
	}
	if err := s.Commit(key, wantHash); err != nil {
		t.Fatalf("正常提交: %v", err)
	}
	if len(s.Committed()) != 1 || s.Committed()[0].Provenance == "" {
		t.Fatal("提交后应可查且带溯源哈希")
	}
}

func TestResumeShortCircuitsCommitted(t *testing.T) {
	root := t.TempDir()
	s, _ := Open(root)
	key := "k2"
	rec, _, _ := s.Prepare(sampleRecord(key))
	segDir := filepath.Join(root, "segments")
	os.MkdirAll(segDir, 0o755)
	os.WriteFile(filepath.Join(segDir, "seg-1.arc"), []byte("x"), 0o644)
	h, _ := HashFile(filepath.Join(segDir, "seg-1.arc"))
	s.Commit(key, h)

	// 重新打开（模拟续跑），同幂等键 Prepare 应短路返回既有记录
	s2, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	existing, reused, err := s2.Prepare(rec)
	if err != nil || !reused || existing.Status != StatusCommitted {
		t.Fatalf("续跑应短路已提交片段: reused=%v err=%v", reused, err)
	}
	if len(s2.Pending()) != 0 {
		t.Fatal("续跑后不应有 pending 片段")
	}
}

func TestInterruptKeepsCommitted(t *testing.T) {
	root := t.TempDir()
	s, _ := Open(root)
	if err := s.MarkInterrupted(2); err != nil {
		t.Fatal(err)
	}
	s2, _ := Open(root)
	if s2.Checkpoint().InterruptAt != 2 {
		t.Fatal("中断点必须持久化以支持续跑")
	}
}

func TestSafePathRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	cases := []string{
		"../escape.txt",
		"a/../../escape.txt",
		"/etc/passwd",
		"a/../../../tmp/x",
	}
	for _, c := range cases {
		if _, err := SafePath(root, c); model.KindOf(err) != model.KindPathViolation {
			t.Fatalf("路径穿越 %q 应拒绝, got %v", c, err)
		}
	}
	// 合法路径应通过并落在 root 内
	ok, err := SafePath(root, "segments/a.arc")
	if err != nil || ok != filepath.Join(root, "segments/a.arc") {
		t.Fatalf("合法路径被误拒: %v %v", ok, err)
	}
}

func TestProvenanceHashStable(t *testing.T) {
	r := sampleRecord("k")
	h1 := ProvenanceHash(r)
	h2 := ProvenanceHash(r)
	if h1 != h2 || len(h1) != 64 {
		t.Fatal("溯源哈希必须稳定且为 sha256")
	}
	r2 := sampleRecord("k")
	r2.FilePath = "different"
	if ProvenanceHash(r2) == h1 {
		t.Fatal("来源变化必须改变溯源哈希")
	}
}
