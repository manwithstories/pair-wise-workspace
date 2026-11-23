package cache

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPutGetRoundtrip(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d1, err := s.Put("src-a", []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	// 重复写入幂等
	d2, err := s.Put("src-a", []byte("hello"))
	if err != nil || d1 != d2 {
		t.Fatalf("idempotent put: %s vs %s, err=%v", d1, d2, err)
	}
	data, err := s.Get(d1, "src-a")
	if err != nil || string(data) != "hello" {
		t.Fatalf("get: %q err=%v", data, err)
	}
}

func TestDigestTamperQuarantined(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.Put("src-a", []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	// 篡改内容
	if err := os.WriteFile(filepath.Join(dir, d), []byte("evil"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = s.Get(d, "src-a")
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("want ErrDigestMismatch, got %v", err)
	}
	if !s.Quarantined(d) {
		t.Fatal("object not quarantined")
	}
	// 隔离后不再可用
	if _, err := s.Get(d, "src-a"); err == nil {
		t.Fatal("quarantined object still readable")
	}
}

func TestProvenanceMismatchQuarantined(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.Put("src-a", []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Get(d, "src-b")
	if !errors.Is(err, ErrProvenanceMismatch) {
		t.Fatalf("want ErrProvenanceMismatch, got %v", err)
	}
	if !s.Quarantined(d) {
		t.Fatal("object not quarantined")
	}
}

func TestWriteFailureKeepsState(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.Put("src-a", []byte("good"))
	if err != nil {
		t.Fatal(err)
	}
	// 目录只读后写入失败，既有对象不受影响
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o755)
	if _, err := s.Put("src-b", []byte("new")); err == nil {
		t.Fatal("want write error")
	}
	data, err := s.Get(d, "src-a")
	if err != nil || string(data) != "good" {
		t.Fatalf("existing object damaged: %q err=%v", data, err)
	}
}
