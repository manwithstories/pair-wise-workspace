// cache.go provides the digest cache keyed by the input SHA-256. An index is
// serialised to a file named for the hash; a cache hit is valid only when the
// stored hash matches the recomputed input hash and the file decodes cleanly.
// Any mismatch — input changed, file truncated, or a partial write from an
// interrupted run — triggers a rebuild. Writes are atomic: the payload is
// written to a temporary file in the same directory and renamed into place, so
// a concurrent reader never sees a half-written cache.
package index

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// CacheFormatVersion bumps when the on-disk index schema changes; an older
// blob is ignored and rebuilt rather than misread.
const CacheFormatVersion = 1

// cacheEnvelope wraps an index with the hashes needed to validate it.
type cacheEnvelope struct {
	Format  int    `json:"format"`
	KeyHash string `json:"key_hash"`
	Payload *Index `json:"payload"`
}

// cacheName derives the cache filename for an input hash. A short prefix keeps
// the name readable; the full hash is stored inside for verification.
func cacheName(hash string) string {
	return "idx-" + hash[:16] + ".json"
}

// CacheStats reports what a cache lookup did.
type CacheStats struct {
	Hit     bool
	Rebuilt bool
	Reason  string
	Path    string
}

// LoadOrBuild returns the index for input, reading it from dir when a valid
// cache exists for that hash, and otherwise scanning and storing it. It returns
// the index, the location of the cache file, and what happened.
func LoadOrBuild(dir string, input []byte) (*Index, CacheStats, error) {
	hash := Hash(input)
	path := filepath.Join(dir, cacheName(hash))
	stats := CacheStats{Path: path}

	if idx, ok, reason := load(path, hash); ok {
		stats.Hit = true
		return idx, stats, nil
	} else if _, existsErr := os.Stat(path); existsErr == nil {
		// A file existed but did not validate: truncation, a stale hash, or a
		// schema change. Rebuild rather than trust it.
		stats.Rebuilt = true
		stats.Reason = reason
	}

	idx := Run(input)
	if err := store(path, hash, idx); err != nil {
		return idx, stats, err
	}
	return idx, stats, nil
}

// load reads and validates a cache file. ok is false when the file is absent,
// truncated, from another format, or keyed to a different input.
func load(path, wantHash string) (*Index, bool, string) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, "absent"
		}
		return nil, false, fmt.Sprintf("read: %v", err)
	}
	if len(data) == 0 {
		return nil, false, "empty file"
	}
	var env cacheEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, false, fmt.Sprintf("decode: %v", err)
	}
	if env.Format != CacheFormatVersion {
		return nil, false, fmt.Sprintf("format %d", env.Format)
	}
	if env.KeyHash != wantHash {
		return nil, false, "key hash mismatch"
	}
	if env.Payload == nil {
		return nil, false, "missing payload"
	}
	// The payload's own source hash must agree too, guarding against a file
	// renamed under the right name but holding another input's index.
	if env.Payload.Source.SHA256 != wantHash {
		return nil, false, "payload hash mismatch"
	}
	return env.Payload, true, ""
}

// store writes an index atomically. It marshals deterministically (struct
// fields, sorted slices set by the scanner) so the same input reproduces the
// same file bytes.
func store(path, hash string, idx *Index) error {
	env := cacheEnvelope{Format: CacheFormatVersion, KeyHash: hash, Payload: idx}
	// json.Marshal orders map keys, but the index carries only structs and
	// slices with a stable order, so the encoding is byte-stable.
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal cache: %w", err)
	}
	return writeAtomic(path, data)
}

// writeAtomic writes data to a sibling temp file and renames it into place,
// fsyncing the file before the rename. The temp name includes the process id so
// concurrent runs do not collide.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create cache dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		// Best effort: a failed rename leaves the temp file behind.
		if _, statErr := os.Stat(tmpName); statErr == nil {
			os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// FileHash returns the SHA-256 of a cache file's bytes, letting the demo report
// a manifest digest that covers the persisted artifact.
func FileHash(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
