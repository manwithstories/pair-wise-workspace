// Package cache 按内容摘要寻址并校验来源的本地对象缓存。
// 摘要或来源不符的对象会被隔离，绝不返回给调用方。
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// 校验失败分类。
var (
	ErrDigestMismatch     = errors.New("cache: content digest mismatch")
	ErrProvenanceMismatch = errors.New("cache: provenance mismatch")
)

// Meta 是对象的来源元数据。
type Meta struct {
	Source string `json:"source"` // 来源摘要（如输入文件 SHA-256）
	Size   int64  `json:"size"`
}

// Store 是磁盘上的内容寻址缓存。
type Store struct {
	Dir string
}

// New 创建缓存目录（含隔离区）。
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "quarantine"), 0o755); err != nil {
		return nil, err
	}
	return &Store{Dir: dir}, nil
}

// Digest 计算数据的内容摘要。
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (s *Store) path(digest string) string     { return filepath.Join(s.Dir, digest) }
func (s *Store) metaPath(digest string) string { return filepath.Join(s.Dir, digest+".meta") }

// Put 写入对象及其来源元数据，返回内容摘要。同内容重复写入幂等。
func (s *Store) Put(source string, data []byte) (string, error) {
	digest := Digest(data)
	if err := writeAtomic(s.path(digest), data); err != nil {
		return "", err
	}
	meta, err := json.Marshal(Meta{Source: source, Size: int64(len(data))})
	if err != nil {
		return "", err
	}
	if err := writeAtomic(s.metaPath(digest), meta); err != nil {
		return "", err
	}
	return digest, nil
}

// Get 读取对象并校验摘要与来源；任一不符即隔离并返回分类错误。
func (s *Store) Get(digest, wantSource string) ([]byte, error) {
	data, err := os.ReadFile(s.path(digest))
	if err != nil {
		return nil, err
	}
	if Digest(data) != digest {
		s.quarantine(digest)
		return nil, fmt.Errorf("%w: %s", ErrDigestMismatch, digest)
	}
	raw, err := os.ReadFile(s.metaPath(digest))
	if err != nil {
		s.quarantine(digest)
		return nil, fmt.Errorf("%w: missing meta: %s", ErrProvenanceMismatch, digest)
	}
	var meta Meta
	if err := json.Unmarshal(raw, &meta); err != nil || meta.Source != wantSource {
		s.quarantine(digest)
		return nil, fmt.Errorf("%w: %s", ErrProvenanceMismatch, digest)
	}
	return data, nil
}

// quarantine 把对象与元数据移入隔离区（保留可追溯性）。
func (s *Store) quarantine(digest string) {
	q := filepath.Join(s.Dir, "quarantine")
	_ = os.Rename(s.path(digest), filepath.Join(q, digest))
	_ = os.Rename(s.metaPath(digest), filepath.Join(q, digest+".meta"))
}

// Quarantined 报告指定摘要是否处于隔离区。
func (s *Store) Quarantined(digest string) bool {
	_, err := os.Stat(filepath.Join(s.Dir, "quarantine", digest))
	return err == nil
}

// writeAtomic 先写临时文件再重命名，写入失败不破坏既有内容。
func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
