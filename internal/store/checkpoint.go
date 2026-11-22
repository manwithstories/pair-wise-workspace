// Package store persists per-node checkpoints with idempotency keys.
// Commits are atomic (temp file, fsync, rename); a checkpoint whose
// checksum does not match is reported as corrupt and never used.
package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"mediacore/internal/clock"
)

// Key identifies one idempotent computation: the same input, params
// and module version always produce the same key.
type Key struct {
	Node         string
	InputDigest  string
	ParamsDigest string
	Version      string
}

// IdempotencyKey hashes the key parts into one stable token.
func (k Key) IdempotencyKey() string {
	h := sha256.New()
	for _, p := range []string{k.Node, k.InputDigest, k.ParamsDigest, k.Version} {
		fmt.Fprintf(h, "%d:%s;", len(p), p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Checkpoint is the committed state of one pipeline node.
type Checkpoint struct {
	Node     string          `json:"node"`
	Key      string          `json:"key"`
	Payload  json.RawMessage `json:"payload"`
	Checksum string          `json:"checksum"`
}

func checksum(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// Store is a directory of checkpoint files.
type Store struct {
	dir string
}

// Open creates the checkpoint directory if needed.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, clock.Wrap(clock.CodeIO, "store-open", err)
	}
	return &Store{dir: dir}, nil
}

var nodeNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func (s *Store) path(node string) (string, error) {
	if !nodeNameRe.MatchString(node) {
		return "", clock.At(clock.CodePathEscape, "store", "", -1,
			"unsafe node name %q", node)
	}
	return filepath.Join(s.dir, node+".json"), nil
}

// Load reads a checkpoint. A missing checkpoint returns (nil, nil);
// a corrupt one returns a CHECKPOINT_CORRUPT error.
func (s *Store) Load(node string) (*Checkpoint, error) {
	p, err := s.path(node)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, clock.Wrap(clock.CodeIO, "store-load", err)
	}
	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, clock.At(clock.CodeCheckpointCorrupt, "store-load", "", -1,
			"node %s: %v", node, err)
	}
	if cp.Node != node || cp.Checksum != checksum(cp.Payload) {
		return nil, clock.At(clock.CodeCheckpointCorrupt, "store-load", "", -1,
			"node %s: checksum mismatch", node)
	}
	return &cp, nil
}

// Commit atomically writes a checkpoint. If an identical key is already
// committed, nothing is written and written is false (no duplicate
// commits on re-run).
func (s *Store) Commit(node, key string, payload []byte) (written bool, err error) {
	if existing, err := s.Load(node); err == nil && existing != nil && existing.Key == key {
		return false, nil
	}
	p, err := s.path(node)
	if err != nil {
		return false, err
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, payload); err != nil {
		return false, clock.Wrap(clock.CodeIO, "store-commit", err)
	}
	payload = buf.Bytes()
	cp := Checkpoint{Node: node, Key: key, Payload: payload, Checksum: checksum(payload)}
	data, err := json.Marshal(cp)
	if err != nil {
		return false, clock.Wrap(clock.CodeIO, "store-commit", err)
	}
	tmp := p + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return false, clock.Wrap(clock.CodeIO, "store-commit", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return false, clock.Wrap(clock.CodeIO, "store-commit", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return false, clock.Wrap(clock.CodeIO, "store-commit", err)
	}
	if err := f.Close(); err != nil {
		return false, clock.Wrap(clock.CodeIO, "store-commit", err)
	}
	if err := os.Rename(tmp, p); err != nil {
		return false, clock.Wrap(clock.CodeIO, "store-commit", err)
	}
	return true, nil
}

// Remove deletes a checkpoint (used when rolling back a corrupt one).
func (s *Store) Remove(node string) error {
	p, err := s.path(node)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return clock.Wrap(clock.CodeIO, "store-remove", err)
	}
	return nil
}

// List returns the names of all committed nodes.
func (s *Store) List() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, clock.Wrap(clock.CodeIO, "store-list", err)
	}
	var out []string
	for _, e := range entries {
		if name, ok := strings.CutSuffix(e.Name(), ".json"); ok {
			out = append(out, name)
		}
	}
	return out, nil
}
