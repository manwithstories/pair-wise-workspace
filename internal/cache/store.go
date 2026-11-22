// Package cache implements a content-addressed result store. A cache hit is
// never trusted on key alone: every hit revalidates the source identity, the
// input digest and the cached content digest, and any mismatch invalidates
// the entry and forces recomputation.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mediapack/internal/fault"
)

// Identity binds a cache entry to exactly one source.
type Identity struct {
	SourceID    string `json:"source_id"`
	InputSHA256 string `json:"input_sha256"`
}

// Key derives the content-addressed key for an identity.
func (id Identity) Key() string {
	sum := sha256.Sum256([]byte(id.SourceID + "\x00" + id.InputSHA256))
	return hex.EncodeToString(sum[:])
}

// meta is the on-disk sidecar record stored next to each cached blob.
type meta struct {
	SourceID     string `json:"source_id"`
	InputSHA256  string `json:"input_sha256"`
	ContentSHA256 string `json:"content_sha256"`
	Size         int64  `json:"size"`
}

// Store is a directory-backed content-addressed cache.
type Store struct {
	dir string
}

// NewStore opens (creating if needed) a cache directory. Path traversal in
// the supplied directory is rejected up front.
func NewStore(dir string) (*Store, error) {
	clean := filepath.Clean(dir)
	if strings.Contains(clean, "..") {
		return nil, fault.New(fault.KindLimit, "cache", "path traversal rejected")
	}
	if err := os.MkdirAll(clean, 0o755); err != nil {
		return nil, fault.Wrap(err, "cache")
	}
	return &Store{dir: clean}, nil
}

// Get returns a cached blob only if all three checks pass: source identity,
// input digest and the stored content digest recomputed from disk.
func (s *Store) Get(id Identity) ([]byte, bool, error) {
	key := id.Key()
	metaPath, blobPath := s.paths(key)
	raw, err := os.ReadFile(metaPath)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fault.Wrap(err, "cache")
	}
	var m meta
	if err := json.Unmarshal(raw, &m); err != nil {
		_ = s.evict(key)
		return nil, false, fault.At(fault.KindChecksum, "cache", -1, "corrupt cache metadata")
	}
	// Check 1+2: source identity and input digest.
	if m.SourceID != id.SourceID || m.InputSHA256 != id.InputSHA256 {
		_ = s.evict(key)
		return nil, false, nil
	}
	blob, err := os.ReadFile(blobPath)
	if err != nil {
		_ = s.evict(key)
		return nil, false, fault.Wrap(err, "cache")
	}
	// Check 3: recompute the content digest from the actual bytes.
	sum := sha256.Sum256(blob)
	got := hex.EncodeToString(sum[:])
	if got != m.ContentSHA256 || int64(len(blob)) != m.Size {
		_ = s.evict(key)
		return nil, false, fault.At(fault.KindChecksum, "cache", -1,
			"cached content digest mismatch: entry invalidated")
	}
	return blob, true, nil
}

// Put writes a blob atomically and records its provenance metadata.
func (s *Store) Put(id Identity, blob []byte) (string, error) {
	key := id.Key()
	metaPath, blobPath := s.paths(key)
	sum := sha256.Sum256(blob)
	m := meta{
		SourceID: id.SourceID, InputSHA256: id.InputSHA256,
		ContentSHA256: hex.EncodeToString(sum[:]), Size: int64(len(blob)),
	}
	metaBytes, err := json.MarshalIndent(&m, "", "  ")
	if err != nil {
		return "", fault.Wrap(err, "cache")
	}
	if err := atomicWrite(blobPath, blob); err != nil {
		return "", err
	}
	if err := atomicWrite(metaPath, metaBytes); err != nil {
		return "", err
	}
	return key, nil
}

// ContentDigest exposes the digest of a blob for manifest/checkpoint use.
func ContentDigest(blob []byte) string {
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])
}

func (s *Store) paths(key string) (metaPath, blobPath string) {
	return filepath.Join(s.dir, key+".json"), filepath.Join(s.dir, key+".bin")
}

func (s *Store) evict(key string) error {
	metaPath, blobPath := s.paths(key)
	if err := os.Remove(blobPath); err != nil && !os.IsNotExist(err) {
		return fault.Wrap(err, "cache")
	}
	if err := os.Remove(metaPath); err != nil && !os.IsNotExist(err) {
		return fault.Wrap(err, "cache")
	}
	return nil
}

// String keeps the key format visible in logs without dumping full hashes.
func (id Identity) String() string {
	return fmt.Sprintf("src=%s input=%s", id.SourceID, short(id.InputSHA256))
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
