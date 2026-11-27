package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"tsqc/internal/clock"
)

const casVersion = 1

// CacheKey binds a cached product to the exact source and input revision.
type CacheKey struct {
	Source      string `json:"source"`
	InputDigest string `json:"input_sha256"`
	Segment     int    `json:"segment"`
}

// CacheEntry is the manifest of one cached segment product.
type CacheEntry struct {
	Key          CacheKey `json:"key"`
	OutputDigest string   `json:"output_sha256"`
	Size         int      `json:"size"`
	Version      int      `json:"version"`
	Seq          int64    `json:"seq"`
}

// Store is a bounded content-addressed cache rooted at a directory. Objects
// live under objects/, the manifest under index.json.
type Store struct {
	mu      sync.Mutex
	dir     string
	maxSize int64
	entries map[string]CacheEntry
	seq     int64
	Hits    int
	Misses  int
	Evicted int
	BadHits int
}

// OpenStore loads or creates a store. A corrupt manifest starts an empty
// store rather than failing the run.
func OpenStore(dir string, maxSize int64) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "objects"), 0o755); err != nil {
		return nil, clock.FatalError(clock.CodeCacheMismatch, "cas.OpenStore", "%v", err)
	}
	if maxSize <= 0 {
		maxSize = 64 << 20
	}
	s := &Store{dir: dir, maxSize: maxSize, entries: make(map[string]CacheEntry)}
	data, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err == nil {
		var manifest struct {
			Version int          `json:"version"`
			Seq     int64        `json:"seq"`
			Entries []CacheEntry `json:"entries"`
		}
		if json.Unmarshal(data, &manifest) == nil && manifest.Version == casVersion {
			s.seq = manifest.Seq
			for _, e := range manifest.Entries {
				s.entries[manifestKey(e.Key)] = e
			}
		}
	}
	return s, nil
}

func manifestKey(k CacheKey) string {
	return k.Source + "\x00" + k.InputDigest + "\x00" + fmt.Sprint(k.Segment)
}

func (s *Store) objectPath(digest string) string {
	return filepath.Join(s.dir, "objects", digest+".bin")
}

// Put stores segment bytes under key. The manifest records both the input
// and output digests so a later hit can prove it is the same product.
func (s *Store) Put(k CacheKey, data []byte) (CacheEntry, error) {
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.WriteFile(s.objectPath(digest), data, 0o644); err != nil {
		return CacheEntry{}, clock.FatalError(clock.CodeCacheMismatch, "cas.Put", "%v", err)
	}
	s.seq++
	e := CacheEntry{Key: k, OutputDigest: digest, Size: len(data), Version: casVersion, Seq: s.seq}
	s.entries[manifestKey(k)] = e
	if err := s.evictLocked(); err != nil {
		return e, err
	}
	return e, s.saveLocked()
}

// Get returns the cached bytes only when the source, input digest and the
// object's own SHA-256 all still match the manifest.
func (s *Store) Get(k CacheKey) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[manifestKey(k)]
	if !ok {
		s.Misses++
		return nil, false
	}
	if e.Key.Source != k.Source || e.Key.InputDigest != k.InputDigest || e.Key.Segment != k.Segment || e.Version != casVersion {
		s.Misses++
		s.BadHits++
		return nil, false
	}
	data, err := os.ReadFile(s.objectPath(e.OutputDigest))
	if err != nil {
		s.Misses++
		s.BadHits++
		return nil, false
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != e.OutputDigest {
		s.Misses++
		s.BadHits++
		return nil, false
	}
	s.Hits++
	return data, true
}

// evictLocked drops the oldest entries until the cache fits maxSize.
func (s *Store) evictLocked() error {
	var total int64
	list := make([]CacheEntry, 0, len(s.entries))
	for _, e := range s.entries {
		total += int64(e.Size)
		list = append(list, e)
	}
	if total <= s.maxSize {
		return nil
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Seq < list[j].Seq })
	for _, e := range list {
		if total <= s.maxSize {
			break
		}
		delete(s.entries, manifestKey(e.Key))
		total -= int64(e.Size)
		s.Evicted++
		// An object may still be referenced by an identical concurrent key;
		// leave it on disk and let the manifest become the source of truth.
	}
	return nil
}

func (s *Store) saveLocked() error {
	list := make([]CacheEntry, 0, len(s.entries))
	for _, e := range s.entries {
		list = append(list, e)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Seq < list[j].Seq })
	manifest := map[string]any{"version": casVersion, "seq": s.seq, "entries": list}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, "index.json.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, "index.json"))
}

// Stats renders cache counters.
func (s *Store) Stats() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fmt.Sprintf("cache hits=%d misses=%d bad=%d evicted=%d entries=%d", s.Hits, s.Misses, s.BadHits, s.Evicted, len(s.entries))
}

// CorruptObject flips one byte of a cached object, a test hook used by the
// demo fault injection to prove digest validation rejects tampered products.
func (s *Store) CorruptObject(digest string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.objectPath(digest)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return errors.New("empty object")
	}
	data[len(data)/2] ^= 0xff
	return os.WriteFile(path, data, 0o644)
}
