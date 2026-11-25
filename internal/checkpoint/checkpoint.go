// Package checkpoint provides atomic, resumable chunk commit records.
package checkpoint

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"mediqc/internal/media"
)

// Chunk is one immutable submitted output fragment.
type Chunk struct {
	ID     string `json:"id"`
	Index  int    `json:"index"`
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}

// Data is the on-disk checkpoint structure.
type Data struct {
	CaseID string           `json:"case_id"`
	Order  []string         `json:"order"`
	Chunks map[string]Chunk `json:"chunks"`
}

// Store atomically persists committed chunk identities.
type Store struct {
	path string
	data Data
}

// Load reads a checkpoint or creates an empty in-memory store.
func Load(path, caseID string) (*Store, *media.Error) {
	s := &Store{path: path, data: Data{CaseID: caseID, Chunks: map[string]Chunk{}}}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return s, nil
		}
		return nil, &media.Error{Code: media.CodeCheckpoint, Component: "checkpoint", Message: err.Error()}
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return nil, &media.Error{Code: media.CodeCheckpoint, Component: "checkpoint", Message: fmt.Sprintf("corrupt checkpoint: %v", err)}
	}
	if s.data.Chunks == nil {
		s.data.Chunks = map[string]Chunk{}
	}
	if s.data.CaseID == "" {
		s.data.CaseID = caseID
	}
	return s, nil
}

// Has reports whether chunk ID was already committed.
func (s *Store) Has(id string) bool {
	_, ok := s.data.Chunks[id]
	return ok
}

// Chunk returns a committed chunk.
func (s *Store) Chunk(id string) (Chunk, bool) {
	c, ok := s.data.Chunks[id]
	return c, ok
}

// Order returns committed IDs in first-commit order.
func (s *Store) Order() []string { return append([]string(nil), s.data.Order...) }

// Commit records a chunk atomically. A matching replay is a no-op and returns
// duplicate=true rather than adding a second submission.
func (s *Store) Commit(c Chunk) (duplicate bool, err *media.Error) {
	if c.ID == "" || c.SHA256 == "" {
		return false, &media.Error{Code: media.CodeCheckpoint, Component: "checkpoint", Message: "chunk id and hash required"}
	}
	if old, ok := s.data.Chunks[c.ID]; ok {
		if old.SHA256 != c.SHA256 || old.Index != c.Index {
			return false, &media.Error{
				Code: media.CodeCheckpoint, Component: "checkpoint",
				Message: fmt.Sprintf("chunk %s was already committed with different identity", c.ID),
			}
		}
		return true, nil
	}
	s.data.Chunks[c.ID] = c
	s.data.Order = append(s.data.Order, c.ID)
	if err := WriteJSONAtomic(s.path, s.data); err != nil {
		return false, err
	}
	return false, nil
}

// WriteJSONAtomic marshals v and renames a synced temporary file over path.
func WriteJSONAtomic(path string, v any) *media.Error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return &media.Error{Code: media.CodeCheckpoint, Component: "checkpoint", Message: err.Error()}
	}
	return WriteFileAtomic(path, b, 0o644)
}

// WriteFileAtomic writes bytes to a same-directory temp file, fsyncs and renames.
func WriteFileAtomic(path string, b []byte, mode os.FileMode) *media.Error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return &media.Error{Code: media.CodeCheckpoint, Component: "checkpoint", Message: err.Error()}
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return &media.Error{Code: media.CodeCheckpoint, Component: "checkpoint", Message: err.Error()}
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return &media.Error{Code: media.CodeCheckpoint, Component: "checkpoint", Message: err.Error()}
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return &media.Error{Code: media.CodeCheckpoint, Component: "checkpoint", Message: err.Error()}
	}
	if err := tmp.Close(); err != nil {
		return &media.Error{Code: media.CodeCheckpoint, Component: "checkpoint", Message: err.Error()}
	}
	if mode != 0 {
		_ = os.Chmod(tmpName, mode)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return &media.Error{Code: media.CodeCheckpoint, Component: "checkpoint", Message: err.Error()}
	}
	return nil
}
