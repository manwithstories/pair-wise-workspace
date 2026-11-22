// Package index persists per-chunk checkpoints with atomic commits and
// recovers from truncated or corrupted index state. All files stay
// inside one user-specified local directory; path traversal and
// incomplete commits are rejected.
package index

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Checkpoint describes one committed chunk.
type Checkpoint struct {
	Chunk          int    `json:"chunk"`
	InputCursor    int64  `json:"input_cursor"`     // input packets consumed
	OutputCursor   int64  `json:"output_cursor"`    // bytes written to the output file
	ChunkHash      string `json:"chunk_hash"`       // sha256 of the chunk's raw input
	OutputTailHash string `json:"output_tail_hash"` // sha256 of output[:OutputCursor]
}

// Store keeps checkpoints inside one local directory.
type Store struct{ dir string }

// Open creates or opens the checkpoint store rooted at dir.
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("index: empty dir")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: abs}, nil
}

// SafeJoin joins name onto dir, rejecting any path that escapes dir.
func SafeJoin(dir, name string) (string, error) {
	if name == "" || name != filepath.Base(name) || strings.Contains(name, "..") {
		return "", fmt.Errorf("index: unsafe name %q", name)
	}
	p := filepath.Join(dir, name)
	if p != dir && !strings.HasPrefix(p, dir+string(os.PathSeparator)) {
		return "", fmt.Errorf("index: path escapes store: %q", name)
	}
	return p, nil
}

func chunkName(chunk int) string { return fmt.Sprintf("chk_%06d.json", chunk) }

// Path returns the validated path of a chunk's checkpoint file.
func (s *Store) Path(chunk int) (string, error) { return SafeJoin(s.dir, chunkName(chunk)) }

// Commit atomically persists cp (write temp file, then rename), so a
// crash never leaves a half-written checkpoint behind.
func (s *Store) Commit(cp Checkpoint) error {
	p, err := s.Path(cp.Chunk)
	if err != nil {
		return err
	}
	b, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// List returns committed checkpoint numbers in ascending order.
func (s *Store) List() ([]int, error) {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var nums []int
	for _, e := range ents {
		var n int
		if _, err := fmt.Sscanf(e.Name(), "chk_%d.json", &n); err == nil {
			nums = append(nums, n)
		}
	}
	sort.Ints(nums)
	return nums, nil
}

// Load reads one checkpoint; a parse failure reports the file corrupt.
func (s *Store) Load(chunk int) (Checkpoint, error) {
	p, err := s.Path(chunk)
	if err != nil {
		return Checkpoint{}, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return Checkpoint{}, err
	}
	var cp Checkpoint
	if err := json.Unmarshal(b, &cp); err != nil {
		return Checkpoint{}, fmt.Errorf("index: checkpoint %d corrupt: %w", chunk, err)
	}
	return cp, nil
}

// Drop removes one checkpoint file.
func (s *Store) Drop(chunk int) error {
	p, err := s.Path(chunk)
	if err != nil {
		return err
	}
	err = os.Remove(p)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Recover scans checkpoints newest-first and returns the newest one
// accepted by verify, dropping every later (invalid) checkpoint. ok is
// false when no checkpoint survives.
func (s *Store) Recover(verify func(Checkpoint) bool) (cp Checkpoint, ok bool, err error) {
	nums, err := s.List()
	if err != nil {
		return Checkpoint{}, false, err
	}
	last := -1
	for i := len(nums) - 1; i >= 0; i-- {
		c, err := s.Load(nums[i])
		if err == nil && verify(c) {
			last = i
			break
		}
	}
	for _, n := range nums[last+1:] {
		if err := s.Drop(n); err != nil {
			return Checkpoint{}, false, err
		}
	}
	if last < 0 {
		return Checkpoint{}, false, nil
	}
	cp, err = s.Load(nums[last])
	return cp, err == nil, err
}

// HashPrefix returns the sha256 hex digest of the first n bytes of
// path and the number of bytes actually read (short when the file is
// truncated).
func HashPrefix(path string, n int64) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	m, err := io.CopyN(h, f, n)
	if err != nil && err != io.EOF {
		return "", m, err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), m, nil
}
