package cache

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"mediapipe/internal/media"
)

// Key binds a cache artifact to both the exact source bytes and the exact
// encoding/mux configuration.
func Key(sourceSHA256 string, encodeConfig any) (string, error) {
	b, err := json.Marshal(struct {
		Source string `json:"source_sha256"`
		Config any    `json:"encode_config"`
	}{Source: sourceSHA256, Config: encodeConfig})
	if err != nil {
		return "", media.WrapError(media.CodeInvalidInput, "canonical cache key", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

type Entry struct {
	Key            string    `json:"key"`
	SourceSHA256   string    `json:"source_sha256"`
	ConfigSHA256   string    `json:"config_sha256"`
	ArtifactName   string    `json:"artifact_name"`
	ArtifactSHA256 string    `json:"artifact_sha256"`
	SizeBytes      int64     `json:"size_bytes"`
	CreatedAt      time.Time `json:"created_at"`
	LastAccessedAt time.Time `json:"last_accessed_at"`
	Hits           int       `json:"hits"`
}

type Verification struct {
	Hit      bool   `json:"cache_hit"`
	SourceOK bool   `json:"source_ok"`
	SHA256OK bool   `json:"sha256_ok"`
	Entry    *Entry `json:"-"`
}

type Store struct {
	root       string
	maxEntries int
	maxBytes   int64
	mu         sync.Mutex
}

func Open(root string, maxEntries int, maxBytes int64) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, media.NewError(media.CodeInvalidInput, "cache root is empty", nil)
	}
	if maxEntries <= 0 {
		maxEntries = media.DefaultLimits().MaxCacheEntries
	}
	if maxBytes <= 0 {
		maxBytes = 2 << 30
	}
	for _, d := range []string{root, filepath.Join(root, "objects"), filepath.Join(root, "entries")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, media.WrapError(media.CodeIO, "create cache dir", err)
		}
	}
	return &Store{root: root, maxEntries: maxEntries, maxBytes: maxBytes}, nil
}

func (s *Store) entryPath(key string) (string, error) {
	if !isHexKey(key) {
		return "", media.NewError(media.CodePathTraversal, "cache key must be sha256 hex", nil)
	}
	return media.SafeJoin(s.root, filepath.Join("entries", key+".json"))
}

func (s *Store) ArtifactPath(key, name string) (string, error) {
	if !isHexKey(key) {
		return "", media.NewError(media.CodePathTraversal, "cache key must be sha256 hex", nil)
	}
	return media.SafeJoin(s.root, filepath.Join("objects", key+"-"+filepath.Base(name)))
}

// Put atomically stores an artifact and its verification entry.
func (s *Store) Put(key, sourceSHA, configSHA, artifactName string, content []byte) (*Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.entryPath(key); err != nil {
		return nil, err
	}
	artPath, err := s.ArtifactPath(key, artifactName)
	if err != nil {
		return nil, err
	}
	n, sum, err := media.AtomicWriteFile(artPath, bytes.NewReader(content), 0o644)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	e := Entry{Key: key, SourceSHA256: sourceSHA, ConfigSHA256: configSHA,
		ArtifactName: filepath.Base(artifactName), ArtifactSHA256: sum, SizeBytes: n,
		CreatedAt: now, LastAccessedAt: now}
	if err := s.writeEntry(&e); err != nil {
		return nil, err
	}
	if err := s.evictLocked(); err != nil {
		return nil, err
	}
	return &e, nil
}

// VerifyHit validates origin and artifact hash on every hit. A failed entry is
// invalidated so the caller can recompute. Missing entries are a normal miss.
func (s *Store) VerifyHit(key, sourcePath string) (Verification, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ep, err := s.entryPath(key)
	if err != nil {
		return Verification{SourceOK: false, SHA256OK: false}, err
	}
	b, err := os.ReadFile(ep)
	if errors.Is(err, fs.ErrNotExist) {
		return Verification{Hit: false, SourceOK: true, SHA256OK: true}, nil
	}
	if err != nil {
		return Verification{}, media.WrapError(media.CodeIO, "read cache entry", err)
	}
	var e Entry
	if err := json.Unmarshal(b, &e); err != nil {
		_ = s.invalidateLocked(key)
		return Verification{Hit: false, SourceOK: false, SHA256OK: false}, nil
	}
	v := Verification{Hit: true, Entry: &e}
	actualSource, err := media.FileSHA256(sourcePath)
	if err != nil {
		return v, err
	}
	v.SourceOK = actualSource == e.SourceSHA256
	artPath, err := s.ArtifactPath(key, e.ArtifactName)
	if err != nil {
		return v, err
	}
	actualArt, err := media.FileSHA256(artPath)
	if err == nil {
		v.SHA256OK = actualArt == e.ArtifactSHA256
	}
	if !v.SourceOK || !v.SHA256OK || e.Key != key {
		_ = s.invalidateLocked(key)
		v.Hit = false
		return v, nil
	}
	e.Hits++
	e.LastAccessedAt = time.Now().UTC()
	_ = s.writeEntry(&e)
	return v, nil
}

// StrictVerification maps tampering to exit code 3 for audit/fault commands.
func StrictVerification(v Verification, err error) error {
	if err != nil {
		return err
	}
	if !v.SourceOK {
		return media.NewError(media.CodeSourceMismatch, "cached source digest mismatch", nil)
	}
	if !v.SHA256OK {
		return media.NewError(media.CodeChecksumMismatch, "cached artifact sha256 mismatch", nil)
	}
	return nil
}

func (s *Store) Invalidate(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.invalidateLocked(key)
}

func (s *Store) invalidateLocked(key string) error {
	ep, err := s.entryPath(key)
	if err != nil {
		return err
	}
	_ = os.Remove(ep)
	if matches, _ := filepath.Glob(filepath.Join(s.root, "objects", key+"-*")); len(matches) > 0 {
		for _, m := range matches {
			if err := os.Remove(m); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return media.WrapError(media.CodeIO, "remove invalid cache object", err)
			}
		}
	}
	return nil
}

func (s *Store) writeEntry(e *Entry) error {
	p, err := s.entryPath(e.Key)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return media.WrapError(media.CodeCheckpoint, "marshal cache entry", err)
	}
	_, _, err = media.AtomicWriteFile(p, bytes.NewReader(b), 0o644)
	return err
}

func (s *Store) evictLocked() error {
	entries, err := s.listLocked()
	if err != nil {
		return err
	}
	var bytes int64
	for _, e := range entries {
		bytes += e.SizeBytes
	}
	for len(entries) > s.maxEntries || bytes > s.maxBytes {
		oldest := entries[0]
		if err := s.invalidateLocked(oldest.Key); err != nil {
			return err
		}
		bytes -= oldest.SizeBytes
		entries = entries[1:]
	}
	return nil
}

func (s *Store) listLocked() ([]Entry, error) {
	dir := filepath.Join(s.root, "entries")
	ds, err := os.ReadDir(dir)
	if err != nil {
		return nil, media.WrapError(media.CodeIO, "list cache entries", err)
	}
	var out []Entry
	for _, d := range ds {
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, d.Name()))
		if err != nil {
			continue
		}
		var e Entry
		if json.Unmarshal(b, &e) == nil {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastAccessedAt.Before(out[j].LastAccessedAt) })
	return out, nil
}

func isHexKey(k string) bool {
	if len(k) != 64 {
		return false
	}
	_, err := hex.DecodeString(k)
	return err == nil
}
