package cache

import (
	"os"
	"path/filepath"
	"testing"

	"mediad/internal/fault"
	"mediad/internal/model"
)

func seg() model.Segment {
	return model.Segment{
		ID: "trk-1-seg-01", TrackID: "trk-1",
		StartTick: 0, EndTick: 100, Status: model.StatusOK,
		SourceDigest: "source-abc",
	}
}

func TestPutGetRoundtrip(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sg := seg()
	data := []byte("segment-bytes")
	if err := s.Put(sg, data); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, hit, ferr := s.Get(sg)
	if ferr != nil || !hit || string(got) != string(data) {
		t.Fatalf("Get = %q hit=%v fault=%v", got, hit, ferr)
	}
	// 未写入的片段返回 miss 而非错误。
	miss := sg
	miss.ID = "trk-1-seg-99"
	if _, hit, ferr := s.Get(miss); hit || ferr != nil {
		t.Fatalf("missing key must be a clean miss: hit=%v %v", hit, ferr)
	}
}

func TestCacheRejectsSourceAndRangeChange(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	sg := seg()
	if err := s.Put(sg, []byte("data")); err != nil {
		t.Fatal(err)
	}
	changed := sg
	changed.SourceDigest = "source-CHANGED"
	_, hit, ferr := s.Get(changed)
	if hit || ferr == nil || ferr.Code != fault.CodeCacheTampered {
		t.Fatalf("source change must be rejected: hit=%v %v", hit, ferr)
	}
	// 坏条目被清除，重放同一片段变为干净 miss。
	if _, hit, _ := s.Get(changed); hit {
		t.Fatal("invalidated entry must be removed")
	}

	if err := s.Put(sg, []byte("data")); err != nil {
		t.Fatal(err)
	}
	moved := sg
	moved.EndTick = 250
	if _, hit, ferr := s.Get(moved); hit || ferr == nil {
		t.Fatalf("range change must be rejected: hit=%v %v", hit, ferr)
	}
}

func TestCacheDetectsBlobTampering(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)
	sg := seg()
	if err := s.Put(sg, []byte("original-bytes")); err != nil {
		t.Fatal(err)
	}
	// 直接替换 blob 文件内容，模拟磁盘上的缓存篡改。
	var blobPath string
	walkErr := filepath.Walk(filepath.Join(dir, "blobs"),
		func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				blobPath = p
			}
			return nil
		})
	if walkErr != nil || blobPath == "" {
		t.Fatalf("locate blob: %v %s", walkErr, blobPath)
	}
	if err := os.WriteFile(blobPath, []byte("tampered-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, hit, ferr := s.Get(sg)
	if hit || ferr == nil || ferr.Code != fault.CodeCacheTampered {
		t.Fatalf("blob tampering must be detected: hit=%v %v", hit, ferr)
	}
}

func TestPathEscapeRejected(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	bad := seg()
	bad.ID = "../escape"
	if err := s.Put(bad, []byte("x")); err == nil || !fault.IsFatal(err) {
		t.Fatalf("path escape must be fatal-rejected: %v", err)
	}
	if _, _, ferr := s.Get(bad); ferr == nil || ferr.Code != fault.CodePathEscape {
		t.Fatalf("Get escape must return path_escape: %v", ferr)
	}
}

func TestOversizeSegmentRejected(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	big := make([]byte, model.MaxSegmentBytes+1)
	if err := s.Put(seg(), big); err == nil || !fault.IsFatal(err) {
		t.Fatalf("oversize segment must be rejected: %v", err)
	}
}
