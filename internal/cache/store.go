// Package cache 实现内容寻址的片段缓存。
//
// 缓存命中也必须重新核对三件事：来源摘要、片段时间范围、内容摘要。
// 任一不符即拒绝命中并要求重算。所有写入采用“临时文件 + 原子替换”，
// 键名经过路径逃逸校验，拒绝任何指向缓存根之外的访问。
package cache

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"mediad/internal/fault"
	"mediad/internal/fingerprint"
	"mediad/internal/model"
)

// entry 是单个缓存条目的元数据。
type entry struct {
	Key           string `json:"key"`
	TrackID       string `json:"track_id"`
	StartTick     int64  `json:"start_tick"`
	EndTick       int64  `json:"end_tick"`
	SourceDigest  string `json:"source_digest"`
	ContentDigest string `json:"content_digest"`
	Blob          string `json:"blob"`
}

// Store 是文件系统支持的内容寻址缓存。
type Store struct {
	root string
}

// NewStore 打开（必要时创建）缓存根目录及其子目录。
func NewStore(root string) (*Store, error) {
	for _, sub := range []string{"meta", "blobs"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			return nil, fault.Fatal(fault.CodeInvalidInput, "cache.MkdirAll", err)
		}
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fault.Fatal(fault.CodeInvalidInput, "cache.Abs", err)
	}
	return &Store{root: abs}, nil
}

// validateName 拒绝路径分隔符、空名与 .. 逃逸片段。
func validateName(name string) error {
	if name == "" || name == "." || name == ".." ||
		strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return fault.New(fault.KindFatal, fault.CodePathEscape,
			"illegal cache key: "+name)
	}
	return nil
}

func (s *Store) metaPath(key string) (string, error) {
	if err := validateName(key); err != nil {
		return "", err
	}
	return filepath.Join(s.root, "meta", key+".json"), nil
}

func (s *Store) blobPath(digest string) (string, error) {
	if err := validateName(digest); err != nil {
		return "", err
	}
	return filepath.Join(s.root, "blobs", digest), nil
}

// Put 原子写入片段数据与元数据。数据文件以内容摘要命名（内容寻址）。
func (s *Store) Put(seg model.Segment, data []byte) error {
	if err := model.ValidateSegmentBytes(len(data)); err != nil {
		return fault.Fatal(fault.CodeTooLarge, "cache.Put", err)
	}
	contentDigest := fingerprint.SegmentHash(data, seg)
	blob, err := s.blobPath(contentDigest)
	if err != nil {
		return err
	}
	if _, err := os.Stat(blob); errors.Is(err, os.ErrNotExist) {
		if err := atomicWrite(blob, data, 0o644); err != nil {
			return err
		}
	} else if err != nil {
		return fault.Fatal(fault.CodeInvalidInput, "cache.Stat", err)
	}
	mp, err := s.metaPath(seg.ID)
	if err != nil {
		return err
	}
	meta, err := json.Marshal(entry{
		Key: seg.ID, TrackID: seg.TrackID,
		StartTick: seg.StartTick, EndTick: seg.EndTick,
		SourceDigest:  seg.SourceDigest,
		ContentDigest: contentDigest,
		Blob:          filepath.Base(blob),
	})
	if err != nil {
		return fault.Fatal(fault.CodeInvalidInput, "cache.Marshal", err)
	}
	return atomicWrite(mp, meta, 0o644)
}

// Get 在三重核对通过后返回缓存数据；任一不符返回可恢复的
// cache_tampered 故障并清除坏条目，调用方应重算。
func (s *Store) Get(seg model.Segment) ([]byte, bool, *fault.Error) {
	mp, err := s.metaPath(seg.ID)
	if err != nil {
		return nil, false, err.(*fault.Error)
	}
	raw, err := os.ReadFile(mp)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fault.Fatal(fault.CodeInvalidInput, "cache.ReadMeta", err)
	}
	var e entry
	if err := json.Unmarshal(raw, &e); err != nil {
		s.invalidate(mp)
		return nil, false, fault.Recoverable(fault.CodeCacheTampered,
			"cache.Unmarshal", seg.StartTick, seg.EndTick, err)
	}
	reject := func(reason error) ([]byte, bool, *fault.Error) {
		s.invalidate(mp)
		return nil, false, fault.Recoverable(fault.CodeCacheTampered,
			"cache.Get", seg.StartTick, seg.EndTick, reason)
	}
	// 1) 来源摘要核对。
	if e.SourceDigest != seg.SourceDigest {
		return reject(errors.New("source digest mismatch"))
	}
	// 2) 片段时间范围与轨道核对。
	if e.StartTick != seg.StartTick || e.EndTick != seg.EndTick ||
		e.TrackID != seg.TrackID {
		return reject(errors.New("segment range mismatch"))
	}
	// 3) 内容摘要核对：重新读取 blob 并即时哈希，检测磁盘内容被替换。
	blob, err := s.blobPath(e.Blob)
	if err != nil {
		return nil, false, err.(*fault.Error)
	}
	data, err := os.ReadFile(blob)
	if err != nil {
		return reject(err)
	}
	if fingerprint.SegmentHash(data, seg) != e.ContentDigest {
		return reject(errors.New("content digest mismatch"))
	}
	return data, true, nil
}

// invalidate 尽力删除失效元数据；blob 内容寻址可安全保留。
func (s *Store) invalidate(metaPath string) {
	_ = os.Remove(metaPath)
}

// atomicWrite 先写同目录临时文件再 rename，保证读者不会看到半写文件。
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fault.Fatal(fault.CodeInvalidInput, "cache.CreateTemp", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return fault.Fatal(fault.CodeInvalidInput, "cache.Write", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return fault.Fatal(fault.CodeInvalidInput, "cache.Sync", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fault.Fatal(fault.CodeInvalidInput, "cache.Close", err)
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		cleanup()
		return fault.Fatal(fault.CodeInvalidInput, "cache.Chmod", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fault.Fatal(fault.CodeInvalidInput, "cache.Rename", err)
	}
	return nil
}
