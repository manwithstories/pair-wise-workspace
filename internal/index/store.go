package index

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"relay/internal/media"
)

// Store 负责索引文件的原子持久化与失效判定。
type Store struct{ path string }

// NewStore 指定索引文件路径。
func NewStore(path string) *Store { return &Store{path: path} }

// persisted 是磁盘上的 JSON 布局，Entry 切片单独编码。
type persisted struct {
	InputDigest string  `json:"input_digest"`
	ParamDigest string  `json:"param_digest"`
	PacketCount int64   `json:"packet_count"`
	SRCount     int64   `json:"sr_count"`
	PPM         int     `json:"ppm"`
	WrapCount   uint64  `json:"wrap_count"`
	Entries     []Entry `json:"entries"`
}

// Save 先写临时文件再 rename，保证索引要么完整存在、要么不出现。
func (s *Store) Save(idx *Index) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return &media.IOError{Op: "mkdir", Path: s.path, Reason: err.Error()}
	}
	p := persisted{
		InputDigest: idx.InputDigest,
		ParamDigest: idx.ParamDigest,
		PacketCount: idx.PacketCount,
		SRCount:     idx.SRCount,
		PPM:         idx.PPM,
		WrapCount:   idx.WrapCount,
		Entries:     idx.Entries,
	}
	b, err := json.Marshal(&p)
	if err != nil {
		return &media.IOError{Op: "marshal", Path: s.path, Reason: err.Error()}
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return &media.IOError{Op: "write", Path: tmp, Reason: err.Error()}
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return &media.IOError{Op: "rename", Path: s.path, Reason: err.Error()}
	}
	return nil
}

// Load 读取索引；文件不存在返回 ErrNotFound，由调用方触发重建。
func (s *Store) Load() (*Index, error) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, &media.IOError{Op: "read", Path: s.path, Reason: err.Error()}
	}
	var p persisted
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("index %s corrupt: %w", s.path, err)
	}
	return &Index{
		InputDigest: p.InputDigest,
		ParamDigest: p.ParamDigest,
		Entries:     p.Entries,
		PacketCount: p.PacketCount,
		SRCount:     p.SRCount,
		PPM:         p.PPM,
		WrapCount:   p.WrapCount,
	}, nil
}

// ErrNotFound 表示尚无可用索引。
var ErrNotFound = errors.New("index not found")

// Valid 判断磁盘索引是否仍与当前输入和参数匹配；任一摘要不符即失效。
func (s *Store) Valid(idx *Index, inputDigest string, params Params) bool {
	if idx == nil {
		return false
	}
	return idx.InputDigest == inputDigest && idx.ParamDigest == params.Digest()
}

// InputDigest 计算文件内容的 sha256 十六进制摘要，供失效判定与检查点共用。
func InputDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", &media.IOError{Op: "open", Path: path, Reason: err.Error()}
	}
	defer f.Close()
	return hashReader(f)
}
