// Package store persists DAG node checkpoints and pipeline artifacts so a run
// can resume after interruption without producing duplicate outputs.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// ErrDuplicateKey is returned when the same idempotent key is saved twice.
var ErrDuplicateKey = errors.New("checkpoint: duplicate save with identical key")

// Entry is the durable record of one completed node.
type Entry struct {
	NodeID    string `json:"node_id"`
	Key       string `json:"key"`
	Output    string `json:"output,omitempty"`
	UpdatedMS int64  `json:"updated_ms"`
}

// FileStore keeps one JSON file per node under dir and writes atomically via
// temp-file rename, so a crash never leaves a half-written checkpoint.
type FileStore struct {
	dir string
}

// NewFileStore creates the checkpoint directory if needed.
func NewFileStore(dir string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("checkpoint: mkdir %s: %w", dir, err)
	}
	return &FileStore{dir: dir}, nil
}

// Dir exposes the checkpoint directory (used by the resume command).
func (s *FileStore) Dir() string { return s.dir }

func (s *FileStore) path(id string) string {
	sum := sha256.Sum256([]byte(id))
	return filepath.Join(s.dir, hex.EncodeToString(sum[:])+".json")
}

// Load implements dag.CheckpointStore and returns the stored key ("" when no
// record exists yet).
func (s *FileStore) Load(id string) (string, error) {
	e, err := s.LoadEntry(id)
	if err != nil || e == nil {
		return "", err
	}
	return e.Key, nil
}

// LoadEntry returns the full checkpoint record for id, or nil when absent.
func (s *FileStore) LoadEntry(id string) (*Entry, error) {
	data, err := os.ReadFile(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var e Entry
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// Save records id under key. Saving the identical key twice is rejected so
// duplicate production cannot pass silently; a changed key overwrites the
// stale record and signals invalidation.
func (s *FileStore) Save(id, key string) error {
	return s.SaveEntry(Entry{NodeID: id, Key: key, UpdatedMS: time.Now().UnixMilli()})
}

// SaveEntry persists a full record atomically.
func (s *FileStore) SaveEntry(e Entry) error {
	if e.NodeID == "" {
		return errors.New("checkpoint: entry missing node id")
	}
	if existing, err := s.Load(e.NodeID); err != nil {
		return err
	} else if existing == e.Key && existing != "" {
		return fmt.Errorf("%w: node=%s", ErrDuplicateKey, e.NodeID)
	}
	if e.UpdatedMS == 0 {
		e.UpdatedMS = time.Now().UnixMilli()
	}
	data, err := json.MarshalIndent(&e, "", "  ")
	if err != nil {
		return err
	}
	final := s.path(e.NodeID)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

// IDs returns the checkpointed node ids in sorted order.
func (s *FileStore) IDs() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, ent := range entries {
		if filepath.Ext(ent.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, ent.Name()))
		if err != nil {
			return nil, err
		}
		var e Entry
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, err
		}
		ids = append(ids, e.NodeID)
	}
	sort.Strings(ids)
	return ids, nil
}

// Remove deletes one checkpoint (used when a node is deliberately invalidated).
func (s *FileStore) Remove(id string) error {
	err := os.Remove(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
