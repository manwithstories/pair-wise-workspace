// Package checkpoint 续跑状态：绑定清单、源帧与参数摘要。
package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// State 检查点状态。
type State struct {
	ManifestSHA string   `json:"manifest_sha"` // 清单内容摘要
	SourcesSHA  string   `json:"sources_sha"`  // 源帧清单摘要
	ParamsSHA   string   `json:"params_sha"`   // 运行参数摘要
	Completed   int      `json:"completed"`    // 已写出的输出帧数
	FrameSHAs   []string `json:"frame_shas"`   // 已写出帧的内容哈希
}

// Store 管理输出目录下的检查点文件。
type Store struct{ path string }

// NewStore 返回 dir 下检查点文件的存取器。
func NewStore(dir string) *Store { return &Store{path: filepath.Join(dir, "checkpoint.json")} }

// Path 返回检查点文件路径。
func (s *Store) Path() string { return s.path }

// Save 原子写入检查点（临时文件 + 重命名）。
func (s *Store) Save(st *State) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("checkpoint: marshal: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("checkpoint: write: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("checkpoint: rename: %w", err)
	}
	return nil
}

// Load 读取检查点；不存在时返回 (nil, nil)。
func (s *Store) Load() (*State, error) {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("checkpoint: read: %w", err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("checkpoint: corrupt state: %w", err)
	}
	return &st, nil
}

// Validate 校验检查点与当前输入、参数摘要一致，不一致拒绝续跑。
func Validate(st *State, manifestSHA, sourcesSHA, paramsSHA string) error {
	if st.ManifestSHA != manifestSHA {
		return fmt.Errorf("checkpoint: manifest digest mismatch, refuse resume")
	}
	if st.SourcesSHA != sourcesSHA {
		return fmt.Errorf("checkpoint: sources digest mismatch, refuse resume")
	}
	if st.ParamsSHA != paramsSHA {
		return fmt.Errorf("checkpoint: params digest mismatch, refuse resume")
	}
	return nil
}

// Digest 计算任意字符串序列的联合 SHA-256（hex）。
func Digest(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%d:", len(p))
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}
