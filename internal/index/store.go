package index

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	merr "mediaroute/internal/errors"
)

// MaxCacheBytes caps a single cached index file.
const MaxCacheBytes = 8 << 20

// Store persists indexes on disk keyed by Key. Writes are atomic: data is
// written to a temporary file in the same directory and renamed into place.
type Store struct {
	// Dir is the cache directory. It is created on first write.
	Dir string
	// MaxBytes overrides MaxCacheBytes when positive.
	MaxBytes int64
}

// cacheEnvelope is the on-disk format; Key is stored alongside the payload so
// a mismatched key is detected even if the file name collides.
type cacheEnvelope struct {
	Key      string `json:"key"`
	Checksum string `json:"checksum"`
	Index    *Index `json:"index"`
}

// Path returns the file path for a key. The path is derived from the scanner
// version and manifest digest so that a media change maps to the same slot and
// can be detected as stale instead of silently becoming a separate entry. The
// full input digest is still part of the key stored inside the file.
func (s *Store) Path(k Key) string {
	slot := fmt.Sprintf("schema=%d|scan=%s|manifest=%s", SchemaVersion, k.ScannerVersion, k.ManifestDigest)
	sum := sha256.Sum256([]byte(slot))
	return filepath.Join(s.Dir, hex.EncodeToString(sum[:16])+".json")
}

// Load reads a cached index for k. It returns (nil, nil, nil) on a miss and a
// typed error when the cache exists but is corrupt or key-mismatched; callers
// treat any non-nil error as "rebuild safely".
func (s *Store) Load(k Key) (*Index, bool, error) {
	path := s.Path(k)
	st, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, s.corrupt(k, -1, "stat cache: %v", err)
	}
	limit := s.MaxBytes
	if limit <= 0 {
		limit = MaxCacheBytes
	}
	if st.Size() > limit {
		return nil, false, s.corrupt(k, st.Size(), "cache file exceeds %d bytes", limit)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false, s.corrupt(k, -1, "open cache: %v", err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, false, s.corrupt(k, -1, "read cache: %v", err)
	}
	if int64(len(raw)) > limit {
		return nil, false, s.corrupt(k, st.Size(), "cache grew beyond %d bytes while reading", limit)
	}
	var env cacheEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, false, s.corrupt(k, 0, "decode cache: %v", err)
	}
	if env.Key != k.String() {
		return nil, false, s.corrupt(k, 0, "cache key mismatch: stored %q", env.Key)
	}
	if env.Index == nil {
		return nil, false, s.corrupt(k, 0, "cache has no index payload")
	}
	if err := verifyEnvelope(&env); err != nil {
		return nil, false, s.corrupt(k, 0, "%v", err)
	}
	if env.Index.InputDigest != k.InputDigest {
		return nil, false, s.stale(k, 0, "media digest changed: cached input=%s current=%s",
			short(env.Index.InputDigest), short(k.InputDigest))
	}
	if env.Index.ManifestDigest != k.ManifestDigest {
		return nil, false, s.stale(k, 0, "manifest digest changed: cached=%s current=%s",
			short(env.Index.ManifestDigest), short(k.ManifestDigest))
	}
	if env.Index.ScannerVersion != k.ScannerVersion {
		return nil, false, s.stale(k, 0, "scanner version changed: cached %q current %q", env.Index.ScannerVersion, k.ScannerVersion)
	}
	want, err := env.Index.ComputeSummary()
	if err != nil {
		return nil, false, s.corrupt(k, 0, "recompute summary: %v", err)
	}
	if want != env.Index.Summary {
		return nil, false, s.corrupt(k, 0, "index summary mismatch: stored %s computed %s",
			short(env.Index.Summary), short(want))
	}
	return env.Index, true, nil
}

// Save atomically writes ix under k.
func (s *Store) Save(k Key, ix *Index) error {
	if ix == nil {
		return merr.New(merr.KindIndex, 0, -1, -1, false, "refusing to cache nil index")
	}
	if ix.InputDigest != k.InputDigest || ix.ManifestDigest != k.ManifestDigest || ix.ScannerVersion != k.ScannerVersion {
		return merr.New(merr.KindIndex, 0, -1, -1, false, "index does not match cache key")
	}
	env := cacheEnvelope{Key: k.String(), Index: ix}
	data, err := json.Marshal(&env)
	if err != nil {
		return merr.New(merr.KindIndex, 0, -1, -1, false, "encode cache: %v", err)
	}
	// Seal the payload portion only, so the checksum does not depend on the
	// checksum itself. The key string is verified independently on load.
	sum, err := sealEnvelope(&cacheEnvelope{Index: ix})
	if err != nil {
		return merr.New(merr.KindIndex, 0, -1, -1, false, "seal cache: %v", err)
	}
	env.Checksum = sum
	data, err = json.Marshal(&env)
	if err != nil {
		return merr.New(merr.KindIndex, 0, -1, -1, false, "encode cache: %v", err)
	}
	limit := s.MaxBytes
	if limit <= 0 {
		limit = MaxCacheBytes
	}
	if int64(len(data)) > limit {
		return merr.New(merr.KindIndex, 0, -1, -1, false, "cache payload %d bytes exceeds limit %d", len(data), limit)
	}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return merr.New(merr.KindIndex, 0, -1, -1, false, "create cache dir: %v", err)
	}
	tmp, err := os.CreateTemp(s.Dir, ".index-*.tmp")
	if err != nil {
		return merr.New(merr.KindIndex, 0, -1, -1, false, "create temp cache: %v", err)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()
	if _, err := tmp.Write(data); err != nil {
		return merr.New(merr.KindIndex, 0, -1, -1, false, "write temp cache: %v", err)
	}
	if err := tmp.Sync(); err != nil {
		return merr.New(merr.KindIndex, 0, -1, -1, false, "sync temp cache: %v", err)
	}
	if err := tmp.Close(); err != nil {
		return merr.New(merr.KindIndex, 0, -1, -1, false, "close temp cache: %v", err)
	}
	if err := os.Rename(tmpName, s.Path(k)); err != nil {
		return merr.New(merr.KindIndex, 0, -1, -1, false, "publish cache: %v", err)
	}
	return nil
}

// Invalidate removes a cache entry, tolerating absence.
func (s *Store) Invalidate(k Key) error {
	if err := os.Remove(s.Path(k)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return merr.New(merr.KindIndex, 0, -1, -1, false, "remove cache: %v", err)
	}
	return nil
}

func (s *Store) corrupt(k Key, off int64, format string, args ...any) *merr.Error {
	return merr.New(merr.KindIndex, 0, -1, off, true, "cache %s: "+format, append([]any{short(k.InputDigest)}, args...)...)
}

func (s *Store) stale(k Key, off int64, format string, args ...any) *merr.Error {
	return merr.New(merr.KindIndex, 0, -1, off, true, "stale cache %s: "+format, append([]any{short(k.InputDigest)}, args...)...)
}

func sealEnvelope(env *cacheEnvelope) (string, error) {
	cp := cacheEnvelope{
		Index: cloneIndexNoSummary(env.Index),
	}
	data, err := json.Marshal(&cp)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func cloneIndexNoSummary(ix *Index) *Index {
	if ix == nil {
		return nil
	}
	cp := *ix
	cp.Summary = ""
	if cp.Tracks == nil {
		cp.Tracks = []Track{}
	}
	for i := range cp.Tracks {
		if cp.Tracks[i].Samples == nil {
			cp.Tracks[i].Samples = []Sample{}
		}
		if cp.Tracks[i].Keyframes == nil {
			cp.Tracks[i].Keyframes = []int64{}
		}
	}
	if cp.CorruptRanges == nil {
		cp.CorruptRanges = []CorruptRange{}
	}
	return &cp
}

func verifyEnvelope(env *cacheEnvelope) error {
	if env.Checksum == "" {
		return fmt.Errorf("cache checksum missing")
	}
	if env.Index == nil {
		return fmt.Errorf("cache has no index payload")
	}
	got, err := sealEnvelope(&cacheEnvelope{Index: env.Index})
	if err != nil {
		return fmt.Errorf("recompute cache checksum: %v", err)
	}
	if got != env.Checksum {
		return fmt.Errorf("cache checksum mismatch")
	}
	return nil
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// Equal compares two indexes structurally by their summaries.
func Equal(a, b *Index) bool {
	return a != nil && b != nil && a.Summary != "" && a.Summary == b.Summary
}

// MarshalCanonical renders an index deterministically (sorted keys, no HTML
// escaping) for reproducible artifact digests.
func MarshalCanonical(ix *Index) ([]byte, error) {
	if ix == nil {
		return nil, fmt.Errorf("index: nil")
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(ix); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
