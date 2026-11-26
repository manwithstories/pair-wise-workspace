// Package cache implements a content-addressable store for restored segments.
// A cache entry is trusted only when three independent proofs all match:
// the rendition/source identifier, a digest of the source bytes the segment
// was derived from, and the SHA-256 of the stored output. Any mismatch invalid
// the entry and forces a rebuild rather than serving unverified bytes.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/archive/tsrestore/internal/timeline"
)

// Key identifies one cacheable segment independent of its output bytes.
type Key struct {
	Rendition  string // rendition/source identifier
	SegmentID  string // stable segment id within the rendition
	SourceHash string // SHA-256 of the source TS bytes the segment is built from
}

// Entry is the on-disk provenance record stored next to the payload.
type Entry struct {
	Rendition  string `json:"rendition"`
	SegmentID  string `json:"segment_id"`
	SourceHash string `json:"source_hash"`
	OutputHash string `json:"output_hash"`
	Size       int    `json:"size"`
}

// Store is a filesystem-backed CAS. It is safe for single-process use; the
// pipeline drives one store for all renditions.
type Store struct {
	root string
}

// Open creates (or opens) a CAS rooted at dir, creating it if needed.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, timeline.NewError(timeline.ClassRetryable, "cache.open",
			"cannot create cache directory", err)
	}
	return &Store{root: dir}, nil
}

// digest returns the lowercase hex SHA-256 of data.
func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// paths returns the payload and metadata paths for a key. The payload name is
// derived from the source hash so different source content never collides.
func (s *Store) paths(k Key) (payload, meta string) {
	name := k.Rendition + "-" + k.SegmentID + "-" + shortName(k.SourceHash)
	return filepath.Join(s.root, name + ".ts"),
		filepath.Join(s.root, name + ".json")
}

func shortName(hash string) string {
	if len(hash) >= 16 {
		return hash[:16]
	}
	return hash
}

// Put writes payload and its provenance record atomically (temp file + rename)
// and returns the output hash. The caller supplies the source hash the bytes
// were derived from.
func (s *Store) Put(k Key, payload []byte) (string, error) {
	if k.Rendition == "" || k.SegmentID == "" || k.SourceHash == "" {
		return "", timeline.NewError(timeline.ClassFatal, "cache.put",
			"incomplete cache key", nil)
	}
	outHash := digest(payload)
	entry := Entry{
		Rendition:  k.Rendition,
		SegmentID:  k.SegmentID,
		SourceHash: k.SourceHash,
		OutputHash: outHash,
		Size:       len(payload),
	}
	payloadPath, metaPath := s.paths(k)
	if err := atomicWrite(payloadPath, payload); err != nil {
		return "", err
	}
	metaBytes, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return "", timeline.NewError(timeline.ClassRetryable, "cache.put",
			"cannot encode metadata", err)
	}
	if err := atomicWrite(metaPath, metaBytes); err != nil {
		return "", err
	}
	return outHash, nil
}

// Get verifies and returns a cached payload. It returns ErrIntegrity (wrapped
// in a classified timeline.Error) when the entry exists but fails any of the
// three provenance checks; the invalid files are removed so the next Put
// rebuilds cleanly. A cache miss is a nil payload with no error.
func (s *Store) Get(k Key) ([]byte, bool, error) {
	payloadPath, metaPath := s.paths(k)
	metaBytes, err := os.ReadFile(metaPath)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, timeline.NewError(timeline.ClassRetryable, "cache.get",
			"cannot read metadata", err)
	}
	var e Entry
	if jerr := json.Unmarshal(metaBytes, &e); jerr != nil {
		return s.invalidate(k, "metadata is not valid JSON")
	}
	// Check 1: source/segment identity.
	if e.Rendition != k.Rendition || e.SegmentID != k.SegmentID {
		return s.invalidate(k, "source identifier mismatch")
	}
	// Check 2: source digest.
	if e.SourceHash != k.SourceHash {
		return s.invalidate(k, "source digest mismatch")
	}
	payload, rerr := os.ReadFile(payloadPath)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return s.invalidate(k, "payload missing")
		}
		return nil, false, timeline.NewError(timeline.ClassRetryable, "cache.get",
			"cannot read payload", rerr)
	}
	// Check 3: output SHA-256 over the actual stored bytes.
	if got := digest(payload); got != e.OutputHash || e.Size != len(payload) {
		return s.invalidate(k, "output sha256 mismatch")
	}
	return payload, true, nil
}

// invalidate removes a failed entry and reports the integrity failure.
func (s *Store) invalidate(k Key, reason string) ([]byte, bool, error) {
	payloadPath, metaPath := s.paths(k)
	_ = os.Remove(payloadPath)
	_ = os.Remove(metaPath)
	return nil, false, timeline.IntegrityError("cache.get", reason, nil)
}

// atomicWrite writes data to a temp file in the same directory then renames
// it over the target, so a crash never leaves a partially visible entry.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return timeline.NewError(timeline.ClassRetryable, "cache.write",
			"cannot create temp file", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return timeline.NewError(timeline.ClassRetryable, "cache.write",
			"write failed", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return timeline.NewError(timeline.ClassRetryable, "cache.write",
			"fsync failed", err)
	}
	if err := tmp.Close(); err != nil {
		return timeline.NewError(timeline.ClassRetryable, "cache.write",
			"close failed", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return timeline.NewError(timeline.ClassRetryable, "cache.write",
			"rename failed", err)
	}
	return nil
}
