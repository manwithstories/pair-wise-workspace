// Package cache implements provenance-checked content-addressed fragment cache.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/pairwise/relay/internal/media"
)

// MaxEntryBytes bounds a single cached fragment.
const MaxEntryBytes = 32 << 20

var safePart = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Key is the logical cache identity and required provenance.
type Key struct {
	SourceNode string `json:"source_node"`
	StreamID   string `json:"stream_id"`
	Rendition  string `json:"rendition"`
	Segment    uint64 `json:"segment"`
}

// Meta is the atomically stored cache reference/provenance record.
type Meta struct {
	Key    Key    `json:"key"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Schema string `json:"schema"`
}

// Cache is a root-directory content-addressed store.
type Cache struct {
	root        string
	mu          sync.Mutex
	rehashes    int64
	hits        int64
	misses      int64
	quarantined int64
}

// Open creates the cache layout under root.
func Open(root string) (*Cache, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, media.NewExitError(4, fmt.Errorf("cache path: %w", err))
	}
	if err := os.MkdirAll(filepath.Join(abs, "objects"), 0o750); err != nil {
		return nil, media.NewExitError(4, err)
	}
	if err := os.MkdirAll(filepath.Join(abs, "refs"), 0o750); err != nil {
		return nil, media.NewExitError(4, err)
	}
	if err := os.MkdirAll(filepath.Join(abs, "quarantine"), 0o750); err != nil {
		return nil, media.NewExitError(4, err)
	}
	return &Cache{root: abs}, nil
}

// Root returns the absolute cache root.
func (c *Cache) Root() string { return c.root }

// Put hashes data, writes the CAS object and atomically updates the reference.
func (c *Cache) Put(key Key, data []byte) (Meta, error) {
	if err := ValidateKey(key); err != nil {
		return Meta{}, err
	}
	if len(data) == 0 {
		return Meta{}, media.NewFault(media.CodeInvalidArgument, fragmentID(key), -1, false, "refusing empty cache entry")
	}
	if len(data) > MaxEntryBytes {
		return Meta{}, media.NewFault(media.CodeInputTooLarge, fragmentID(key), 0, false, "cache entry %d > limit %d", len(data), MaxEntryBytes)
	}
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	meta := Meta{Key: key, SHA256: sha, Size: int64(len(data)), Schema: "relay-cas-v1"}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.rehashes++
	objPath := c.objectPath(sha)
	if err := atomicWrite(objPath, data, 0o640); err != nil {
		return Meta{}, err
	}
	refPath, err := c.refPath(key)
	if err != nil {
		return Meta{}, err
	}
	raw, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return Meta{}, err
	}
	if err := atomicWrite(refPath, raw, 0o640); err != nil {
		return Meta{}, err
	}
	return meta, nil
}

// Get loads and fully verifies a cache entry. Any provenance or digest failure
// quarantines the untrusted entry and returns a classified Fault; callers must
// reprocess and Put a fresh entry.
func (c *Cache) Get(want Key) ([]byte, Meta, error) {
	if err := ValidateKey(want); err != nil {
		return nil, Meta{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	refPath, err := c.refPath(want)
	if err != nil {
		return nil, Meta{}, err
	}
	raw, err := os.ReadFile(refPath)
	if errors.Is(err, os.ErrNotExist) {
		c.misses++
		return nil, Meta{}, media.NewFault(media.CodeCacheDigest, fragmentID(want), -1, true, "cache miss")
	}
	if err != nil {
		return nil, Meta{}, media.NewExitError(4, err)
	}
	var meta Meta
	if err := json.Unmarshal(raw, &meta); err != nil {
		_ = c.quarantineFiles(refPath, "", nil, media.CodeCheckpoint)
		return nil, Meta{}, media.NewFault(media.CodeCheckpoint, fragmentID(want), -1, true, "bad cache metadata: %v", err)
	}

	if meta.Key.SourceNode != want.SourceNode || meta.Key.StreamID != want.StreamID ||
		meta.Key.Segment != want.Segment || meta.Key.Rendition != want.Rendition {
		f := media.NewFault(media.CodeCacheOrigin, fragmentID(want), -1, true,
			"provenance mismatch: cached source=%q stream=%q rendition=%q segment=%d; want source=%q stream=%q rendition=%q segment=%d",
			meta.Key.SourceNode, meta.Key.StreamID, meta.Key.Rendition, meta.Key.Segment,
			want.SourceNode, want.StreamID, want.Rendition, want.Segment)
		_ = c.quarantineFiles(refPath, c.objectPath(meta.SHA256), nil, media.CodeCacheOrigin)
		c.quarantined++
		c.misses++
		return nil, Meta{}, f
	}

	obj, err := os.ReadFile(c.objectPath(meta.SHA256))
	if err != nil {
		_ = c.quarantineFiles(refPath, "", nil, media.CodeCacheDigest)
		c.quarantined++
		c.misses++
		return nil, Meta{}, media.NewFault(media.CodeCacheDigest, fragmentID(want), -1, true, "cache object missing: %v", err)
	}
	c.rehashes++
	sum := sha256.Sum256(obj)
	actual := hex.EncodeToString(sum[:])
	if actual != meta.SHA256 || int64(len(obj)) != meta.Size {
		_ = c.quarantineFiles(refPath, c.objectPath(meta.SHA256), obj, media.CodeCacheDigest)
		c.quarantined++
		c.misses++
		return nil, Meta{}, media.NewFault(media.CodeCacheDigest, fragmentID(want), -1, true,
			"digest/size mismatch: meta_sha256=%s actual_sha256=%s meta_size=%d actual_size=%d",
			meta.SHA256, actual, meta.Size, len(obj))
	}
	c.hits++
	return obj, meta, nil
}

// Stats reports cache verification counters.
type Stats struct {
	Rehashes    int64 `json:"rehash"`
	Hits        int64 `json:"hits"`
	Misses      int64 `json:"misses"`
	Quarantined int64 `json:"quarantined"`
}

// Stats returns a snapshot.
func (c *Cache) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Stats{Rehashes: c.rehashes, Hits: c.hits, Misses: c.misses, Quarantined: c.quarantined}
}

// RehashCount is retained as a convenience for CLI output.
func (c *Cache) RehashCount() int64 { return c.Stats().Rehashes }

// ValidateKey rejects empty, oversized or path-escaping provenance fields.
func ValidateKey(k Key) error {
	for name, v := range map[string]string{
		"source": k.SourceNode, "stream": k.StreamID, "rendition": k.Rendition,
	} {
		if !safePart.MatchString(v) {
			return media.NewFault(media.CodeUnsafePath, fragmentID(k), -1, false, "invalid cache %s %q", name, v)
		}
	}
	if k.Segment == 0 || k.Segment > 1_000_000 {
		return media.NewFault(media.CodeInvalidArgument, fragmentID(k), -1, false, "cache segment out of range: %d", k.Segment)
	}
	return nil
}

func (c *Cache) objectPath(sha string) string {
	if len(sha) < 2 {
		sha = strings.Repeat("0", 64)
	}
	return filepath.Join(c.root, "objects", sha[:2], sha+".bin")
}

func (c *Cache) refPath(k Key) (string, error) {
	if err := ValidateKey(k); err != nil {
		return "", err
	}
	p := filepath.Join(c.root, "refs", k.StreamID, k.Rendition, fmt.Sprintf("segment-%06d.json", k.Segment))
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", media.NewExitError(4, err)
	}
	rel, err := filepath.Rel(filepath.Join(c.root, "refs"), abs)
	if err != nil || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return "", media.NewFault(media.CodeUnsafePath, fragmentID(k), -1, false, "unsafe cache reference path")
	}
	return abs, nil
}

func (c *Cache) quarantineFiles(refPath, objectPath string, objectBytes []byte, reason media.Code) error {
	qdir := filepath.Join(c.root, "quarantine")
	_ = os.MkdirAll(qdir, 0o750)
	base := fmt.Sprintf("%s-%d", reason, sha256.Sum256([]byte(refPath+objectPath)))
	base = hex.EncodeToString([]byte(base))[:16]
	if refPath != "" {
		dst := filepath.Join(qdir, base+".ref.json")
		_ = os.Rename(refPath, dst)
	}
	if objectPath != "" {
		dst := filepath.Join(qdir, base+".object")
		if len(objectBytes) > 0 {
			_ = os.WriteFile(dst, objectBytes, 0o600)
		} else {
			_ = os.Rename(objectPath, dst)
		}
	}
	return nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func fragmentID(k Key) media.FragmentID {
	return media.FragmentID{Rendition: k.Rendition, Segment: k.Segment}
}
