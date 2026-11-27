package journal

import (
	"os"
	"path/filepath"
	"testing"

	"archivemedia/internal/media"
)

func sampleCP(seq uint64) *Checkpoint {
	return &Checkpoint{
		Seq:             seq,
		LastExtSeq:      int64(seq) * 100,
		WatermarkMicros: int64(seq) * 5000,
		CountedPackets:  seq * 10,
		OutputOffset:    int64(seq) * 2048,
		InputDigest:     DigestHex([]byte("input-stream")),
		Payload:         []byte{0xDE, 0xAD, 0xBE, 0xEF},
	}
}

func TestCheckpointSaveLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	cp := sampleCP(1)
	if err := s.Save(cp); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Seq != 1 || got.LastExtSeq != 100 || got.CountedPackets != 10 ||
		got.OutputOffset != 2048 || got.InputDigest != cp.InputDigest ||
		len(got.Payload) != 4 {
		t.Fatalf("检查点往返字段丢失: %+v", got)
	}
}

func TestCheckpointRotation(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)
	if err := s.Save(sampleCP(1)); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(sampleCP(2)); err != nil {
		t.Fatal(err)
	}
	// current 应为 seq=2，previous 应为 seq=1。
	cur, err := readCheckpoint(filepath.Join(dir, "checkpoint.current"))
	if err != nil || cur.Seq != 2 {
		t.Fatalf("current 异常: %+v err=%v", cur, err)
	}
	prev, err := readCheckpoint(filepath.Join(dir, "checkpoint.previous"))
	if err != nil || prev.Seq != 1 {
		t.Fatalf("previous 异常: %+v err=%v", prev, err)
	}
}

func TestCheckpointTruncatedFallsBack(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)
	if err := s.Save(sampleCP(1)); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(sampleCP(2)); err != nil {
		t.Fatal(err)
	}
	// 截断 current，Load 必须回退到 previous(seq=1)。
	curPath := filepath.Join(dir, "checkpoint.current")
	raw, err := os.ReadFile(curPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(curPath, raw[:cpHeaderLen-2], 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatalf("截断后应回退，得到错误: %v", err)
	}
	if got.Seq != 1 {
		t.Fatalf("回退检查点 seq=%d want 1", got.Seq)
	}
}

func TestCheckpointCorruptSHAFallsBack(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)
	_ = s.Save(sampleCP(1))
	_ = s.Save(sampleCP(2))
	curPath := filepath.Join(dir, "checkpoint.current")
	raw, _ := os.ReadFile(curPath)
	raw[len(raw)-1] ^= 0xFF // 破坏载荷末字节，SHA 必失败
	if err := os.WriteFile(curPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil || got.Seq != 1 {
		t.Fatalf("SHA 损坏回退失败: %+v err=%v", got, err)
	}
}

func TestCheckpointBothInvalid(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)
	_ = s.Save(sampleCP(1))
	_ = s.Save(sampleCP(2))
	for _, name := range []string{"checkpoint.current", "checkpoint.previous"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte{0x00, 0x01, 0x02}, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Load(); !media.IsKind(err, media.KindCheckpoint) {
		t.Fatalf("双坏点应为 KindCheckpoint: %v", err)
	}
}

func TestCheckpointEmptyDir(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)
	if _, err := s.Load(); !media.IsKind(err, media.KindCheckpoint) {
		t.Fatalf("无检查点应报 KindCheckpoint: %v", err)
	}
}

func TestCheckpointRejectsNil(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	if err := s.Save(nil); !media.IsKind(err, media.KindArgument) {
		t.Fatalf("nil 检查点应报参数错误: %v", err)
	}
}

func TestDigestHexStable(t *testing.T) {
	if DigestHex([]byte("abc")) != DigestHex([]byte("abc")) {
		t.Fatal("摘要不确定")
	}
	if got := DigestHex(nil); len(got) != 64 {
		t.Fatalf("空摘要长度 = %d want 64", len(got))
	}
}
