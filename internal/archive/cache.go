// Package archive implements the content-addressable fragment cache and the
// atomic checkpoint store. Both are confined to a single archive root and
// never touch source media: cached outputs are verified against their source
// digest on every hit, and checkpoints are committed only after an output has
// landed and passed verification.
package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"archivecheck/internal/media"
)

// CacheKeyInput is everything that determines an archive fragment's content.
// Changing any field changes the key, so a stale mapping, edit boundary or
// format revision can never be served as a cache hit.
type CacheKeyInput struct {
	SourceSHA256 string // digest of the full source package
	Mapping      string // canonical track-mapping + edit-boundary string
	Format       string // demux.FormatVersion
}

// CacheEntry is the on-disk record next to a cached fragment.
type CacheEntry struct {
	Key          string `json:"key"`
	FragmentPath string `json:"fragment_path"`
	FragmentSHA  string `json:"fragment_sha256"`
	SourceSHA    string `json:"source_sha256"`
	Format       string `json:"format"`
	Samples      int    `json:"samples"`
}

// Cache is a content-addressable store rooted at <root>/cache. All restored
// outputs are constrained to the enclosing archive root.
type Cache struct {
	archiveRoot string
	root        string
}

// NewCache opens (creating on first use) the cache under root.
func NewCache(root string) (*Cache, error) {
	archiveRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return nil, media.NewFault(media.KindIO, "", 0, -1, "resolve root: %s", err)
	}
	c := &Cache{archiveRoot: archiveRoot, root: filepath.Join(archiveRoot, "cache")}
	if err := os.MkdirAll(c.root, 0o755); err != nil {
		return nil, media.NewFault(media.KindIO, "", 0, -1, "cache mkdir: %s", err)
	}
	return c, nil
}

// Key derives the hex cache key from the bound inputs.
func Key(in CacheKeyInput) string {
	h := sha256.New()
	fmt.Fprintf(h, "format=%s\nsource=%s\nmapping=%s\n", in.Format, in.SourceSHA256, in.Mapping)
	return hex.EncodeToString(h.Sum(nil))
}

func (c *Cache) blobPath(key string) string {
	// Shard by the first two hex chars to keep directories small.
	return filepath.Join(c.root, key[:2], key)
}

func (c *Cache) metaPath(key string) string { return c.blobPath(key) + ".json" }

// Put stores a produced fragment and its metadata. The fragment is copied into
// the cache via a temp file + rename, so a crash never leaves a partial blob.
func (c *Cache) Put(entry CacheEntry) error {
	if err := validateHex(entry.Key, 64); err != nil {
		return err
	}
	dir := filepath.Dir(c.blobPath(entry.Key))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return media.NewFault(media.KindIO, "", 0, -1, "cache shard mkdir: %s", err)
	}
	// The source fragment path is supplied by the pipeline, which confines all
	// outputs to the archive root; the cache itself only writes blobs under its
	// own shard directories.
	blob := c.blobPath(entry.Key)
	if err := copyAtomic(entry.FragmentPath, blob); err != nil {
		return err
	}
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(c.metaPath(entry.Key), data)
}

// Lookup returns the cached entry only if the blob still exists. It does NOT
// verify content here — callers must run VerifyHit, which re-reads both the
// source and the fragment.
func (c *Cache) Lookup(key string) (*CacheEntry, bool, error) {
	if err := validateHex(key, 64); err != nil {
		return nil, false, err
	}
	data, err := os.ReadFile(c.metaPath(key))
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, media.NewFault(media.KindIO, "", 0, -1, "read cache meta: %s", err)
	}
	var entry CacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, false, media.NewFault(media.KindCorruptIndex, "", 0, -1, "cache meta corrupt: %s", err)
	}
	if _, err := os.Stat(c.blobPath(key)); err != nil {
		return nil, false, nil // blob missing: treat as a miss
	}
	return &entry, true, nil
}

// VerifyHit re-reads the source and the cached fragment and recomputes both
// digests. A mismatch (including a deliberately forged record) rejects the hit
// with a located digest fault; the caller must then recompute the fragment.
func (c *Cache) VerifyHit(key, sourcePath string) (*CacheEntry, error) {
	entry, ok, err := c.Lookup(key)
	if err != nil || !ok {
		return entry, err
	}
	srcDigest, err := digestFile(sourcePath)
	if err != nil {
		return nil, err
	}
	if srcDigest != entry.SourceSHA {
		return nil, media.NewFault(media.KindDigestMismatch, filepath.Base(sourcePath), 0, -1,
			"source digest changed: cache=%s actual=%s", entry.SourceSHA, srcDigest)
	}
	fragDigest, err := digestFile(c.blobPath(key))
	if err != nil {
		return nil, err
	}
	if fragDigest != entry.FragmentSHA {
		return nil, media.NewFault(media.KindDigestMismatch, filepath.Base(entry.FragmentPath), 0, -1,
			"cached fragment digest mismatch: record=%s actual=%s; rejecting hit", entry.FragmentSHA, fragDigest)
	}
	return entry, nil
}

// BlobPath exposes the cached fragment's location for restoring an output.
func (c *Cache) BlobPath(key string) string { return c.blobPath(key) }

// Restore copies a verified blob to dst, again via temp+rename. dst must be
// inside the archive root.
func (c *Cache) Restore(key, dst string) error {
	abs, err := filepath.Abs(filepath.Clean(dst))
	if err != nil {
		return media.NewFault(media.KindIO, filepath.Base(dst), 0, -1, "resolve dst: %s", err)
	}
	if !withinRoot(c.archiveRoot, abs) {
		return media.NewFault(media.KindIO, filepath.Base(dst), 0, -1, "restore target escapes archive root")
	}
	return copyAtomic(c.blobPath(key), abs)
}

// DigestSource hashes an entire source file, used both for keys and verification.
func DigestSource(path string) (string, error) { return digestFile(path) }

func digestFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", media.NewFault(media.KindIO, filepath.Base(path), 0, -1, "digest open: %s", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", media.NewFault(media.KindIO, filepath.Base(path), 0, -1, "digest read: %s", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func validateHex(s string, want int) error {
	if len(s) != want {
		return media.NewFault(media.KindDigestMismatch, "", 0, -1, "digest %q has length %d, want %d", s, len(s), want)
	}
	if _, err := hex.DecodeString(s); err != nil {
		return media.NewFault(media.KindDigestMismatch, "", 0, -1, "digest not hex: %s", err)
	}
	return nil
}

// withinRoot reports whether p is contained inside root after cleaning both.
func withinRoot(root, p string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(p))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}
