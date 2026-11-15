// Package shard defines the shard manifest format and helpers to locate
// and validate shard files on disk.
package shard

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Entry describes a single shard inside a manifest.
type Entry struct {
	Index  int    `json:"index"`
	Size   int    `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest lists every shard of a fixture or pack.
type Manifest struct {
	Seed      int64   `json:"seed"`
	ShardSize int     `json:"shard_size"`
	Shards    []Entry `json:"shards"`
}

// ShardName returns the canonical file name for a shard index.
func ShardName(index int) string {
	return fmt.Sprintf("shard-%06d.bin", index)
}

// ShardPath joins dir, the shard subdirectory and the canonical name.
func ShardPath(dir string, index int) string {
	return filepath.Join(dir, "shards", ShardName(index))
}

// Digest computes the manifest digest over the concatenated shard digests.
// It is bound into every shard's AAD so manifest tampering is detectable.
func (m *Manifest) Digest() []byte {
	h := sha256.New()
	for _, e := range m.Shards {
		raw, err := hex.DecodeString(e.SHA256)
		if err != nil {
			return nil
		}
		h.Write(raw)
	}
	return h.Sum(nil)
}

// Entry returns the manifest entry for index.
func (m *Manifest) Entry(index int) (*Entry, error) {
	if index < 0 || index >= len(m.Shards) {
		return nil, fmt.Errorf("shard index %d out of manifest range", index)
	}
	return &m.Shards[index], nil
}

// ValidateLength checks that the file at path has the recorded size.
func (m *Manifest) ValidateLength(path string, index int) error {
	e, err := m.Entry(index)
	if err != nil {
		return err
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.Size() != int64(e.Size) {
		return fmt.Errorf("shard %d length mismatch: got %d want %d", index, st.Size(), e.Size)
	}
	return nil
}

// Load reads and validates a manifest from disk.
func Load(path string) (*Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	for i, e := range m.Shards {
		if e.Index != i {
			return nil, fmt.Errorf("manifest shard order broken at %d", i)
		}
		if _, err := hex.DecodeString(e.SHA256); err != nil {
			return nil, fmt.Errorf("manifest shard %d bad digest: %w", i, err)
		}
	}
	return &m, nil
}

// Save writes the manifest as indented JSON.
func (m *Manifest) Save(path string) error {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}
