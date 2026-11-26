// Package adapter 解析带版本的清单与帧数据，送入媒体内核。
package adapter

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"aircheck/internal/media"
)

// 输入大小与数量上限
const (
	MaxManifestBytes = 64 << 20
	MaxImageBytes    = 16 << 20
	MaxLineBytes     = 1 << 20
	MaxFrames        = 100000
)

// Manifest 解析后的清单。
type Manifest struct {
	Version  int
	Timebase media.Timebase
	Tracks   []Track
	Frames   []FrameRec
	Anchors  []AnchorRec
}

// Track 轨道描述。
type Track struct {
	ID       string
	Priority int // 数值小者优先
	FPS      float64
	Modulus  int64 // PTS 回绕模数，0 表示不回绕
}

// FrameRec 帧记录。
type FrameRec struct {
	Track  string
	Seq    int64
	PTS    int64
	Dur    int64
	SHA256 string
	File   string
}

// AnchorRec 漂移锚点记录。
type AnchorRec struct {
	Track string
	PTS   int64
	RefMs float64
}

// parser 解析清单：first 为已读出的首行（header），sc 指向后续行。
type parser func(first []byte, sc *bufio.Scanner, path string) (*Manifest, error)

var registry = map[int]parser{}

func register(version int, p parser) { registry[version] = p }

// ParseManifest 读取 JSONL 清单并按版本分发；未知版本给出可定位错误。
func ParseManifest(path string, maxBytes int64) (*Manifest, error) {
	if maxBytes <= 0 {
		maxBytes = MaxManifestBytes
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("adapter: open manifest: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, maxBytes+1))
	sc.Buffer(make([]byte, 0, 64<<10), MaxLineBytes)
	if !sc.Scan() {
		return nil, fmt.Errorf("%s:1: empty manifest", path)
	}
	first := sc.Bytes()
	if len(first) > MaxLineBytes {
		return nil, fmt.Errorf("%s:1: line too long", path)
	}
	var head struct {
		Type    string `json:"type"`
		Version int    `json:"version"`
	}
	if err := json.Unmarshal(first, &head); err != nil {
		return nil, fmt.Errorf("%s:1: invalid header: %v", path, err)
	}
	if head.Type != "header" {
		return nil, fmt.Errorf("%s:1: first record must be header, got %q", path, head.Type)
	}
	p, ok := registry[head.Version]
	if !ok {
		return nil, fmt.Errorf("%s:1: unsupported manifest version %d", path, head.Version)
	}
	m, err := p(first, sc, path)
	if err != nil {
		return nil, err
	}
	if len(m.Frames) > MaxFrames {
		return nil, fmt.Errorf("%s: frame count %d exceeds limit %d", path, len(m.Frames), MaxFrames)
	}
	return m, nil
}

// SecureJoin 将相对路径限制在 root 内，拒绝路径穿越。
func SecureJoin(root, rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) {
		return "", fmt.Errorf("adapter: unsafe path %q", rel)
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("adapter: path traversal %q", rel)
	}
	full := filepath.Join(root, clean)
	rootClean := filepath.Clean(root)
	if full != rootClean && !strings.HasPrefix(full, rootClean+string(filepath.Separator)) {
		return "", fmt.Errorf("adapter: path %q escapes root", rel)
	}
	return full, nil
}
