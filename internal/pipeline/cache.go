// Package pipeline coordinates the content-addressed cache, checkpoints and
// atomic commits that make the demo resumable and its output reproducible.
package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrCacheInvalid indicates the cache entry disagreed with the recorded
// source identity, input digest, parameter digest or artefact digest.
var ErrCacheInvalid = errors.New("cache entry invalidated")

// ErrIncomplete indicates a stage was recorded as started but not verified.
var ErrIncomplete = errors.New("stage not completed")

// Artifact is one cached intermediate product.
type Artifact struct {
	Stage         string `json:"stage"`
	SourceID      string `json:"source_id"`
	InputDigest   string `json:"input_digest"`
	ParamsDigest  string `json:"params_digest"`
	ContentDigest string `json:"content_digest"`
	Payload       []byte `json:"payload"`
}

// Cache is a directory-backed content-addressed store. Every file is written
// to a temporary name under the cache root and renamed into place only after
// its bytes are synced, so a partially written file can never be mistaken for
// a completed artefact.
type Cache struct {
	root string
}

// OpenCache creates the cache directory if needed.
func OpenCache(root string) (*Cache, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &Cache{root: root}, nil
}

// Root returns the cache directory.
func (c *Cache) Root() string { return c.root }

func (c *Cache) path(stage, key string) string {
	return filepath.Join(c.root, sanitize(stage)+"-"+key+".json")
}

func sanitize(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch == '/' || ch == '\\' || ch == 0 || ch == '.' {
			out = append(out, '_')
			continue
		}
		out = append(out, ch)
	}
	return string(out)
}

// Key derives a stable cache key from the four identity components.
func Key(sourceID, inputDigest, paramsDigest string) string {
	h := sha256.Sum256([]byte(sourceID + "\x00" + inputDigest + "\x00" + paramsDigest))
	return hex.EncodeToString(h[:])
}

// Store writes an artefact atomically. It returns the content digest.
func (c *Cache) Store(a Artifact) (string, error) {
	a.ContentDigest = digestPayload(a.Payload)
	key := Key(a.SourceID, a.InputDigest, a.ParamsDigest)
	data, err := json.Marshal(a)
	if err != nil {
		return "", err
	}
	if err := writeAtomic(c.path(a.Stage, key), data); err != nil {
		return "", err
	}
	return a.ContentDigest, nil
}

// Load returns a cached artefact only if all four identity components match.
// A mismatch invalidates the entry: it is removed and ErrCacheInvalid is
// returned so the caller recomputes.
func (c *Cache) Load(stage, sourceID, inputDigest, paramsDigest string, hits *int) (Artifact, error) {
	key := Key(sourceID, inputDigest, paramsDigest)
	p := c.path(stage, key)
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Artifact{}, os.ErrNotExist
		}
		return Artifact{}, err
	}
	var a Artifact
	if err := json.Unmarshal(data, &a); err != nil {
		os.Remove(p)
		return Artifact{}, fmt.Errorf("%w: unreadable entry: %v", ErrCacheInvalid, err)
	}
	if a.Stage != stage || a.SourceID != sourceID || a.InputDigest != inputDigest || a.ParamsDigest != paramsDigest {
		os.Remove(p)
		return Artifact{}, fmt.Errorf("%w: identity mismatch", ErrCacheInvalid)
	}
	if want := digestPayload(a.Payload); want != a.ContentDigest {
		os.Remove(p)
		return Artifact{}, fmt.Errorf("%w: payload digest %s != %s", ErrCacheInvalid, want, a.ContentDigest)
	}
	if hits != nil {
		*hits++
	}
	return a, nil
}

func digestPayload(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// writeAtomic writes to a temporary file in the same directory, syncs it, then
// renames. A crash leaves only the temporary file behind.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
