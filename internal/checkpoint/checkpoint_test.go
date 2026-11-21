package checkpoint

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cp.json")
	want := State{InputDigest: "aaa", ParamsDigest: "bbb", FramesDone: 4096, Stage: "segment"}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.InputDigest != want.InputDigest || got.FramesDone != want.FramesDone ||
		got.Stage != want.Stage || got.Version != CurrentVersion {
		t.Fatalf("检查点往返不一致: %+v", got)
	}
}

func TestLoadMissing(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "none.json")); !errors.Is(err, ErrNoCheckpoint) {
		t.Fatalf("缺失检查点应返回 ErrNoCheckpoint, got %v", err)
	}
}

func TestVerifyDigestMismatch(t *testing.T) {
	s := State{Version: CurrentVersion, InputDigest: "aaa", ParamsDigest: "bbb"}
	if err := Verify(s, "aaa", "bbb"); err != nil {
		t.Fatalf("摘要一致时应通过: %v", err)
	}
	if err := Verify(s, "xxx", "bbb"); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("输入摘要不匹配必须拒绝, got %v", err)
	}
	if err := Verify(s, "aaa", "xxx"); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("参数摘要不匹配必须拒绝, got %v", err)
	}
	old := s
	old.Version = 0
	if err := Verify(old, "aaa", "bbb"); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("版本不符必须拒绝, got %v", err)
	}
}

func TestSaveDeterministicContent(t *testing.T) {
	dir := t.TempDir()
	s := State{InputDigest: "d", ParamsDigest: "p", FramesDone: 12, Stage: "ladder"}
	p1, p2 := filepath.Join(dir, "a.json"), filepath.Join(dir, "b.json")
	if err := Save(p1, s); err != nil {
		t.Fatal(err)
	}
	if err := Save(p2, s); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(p1)
	b, _ := os.ReadFile(p2)
	if string(a) != string(b) {
		t.Fatal("相同状态的检查点文件内容必须一致")
	}
}
