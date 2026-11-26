package index

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"mediaflow/internal/fixture"
	"mediaflow/internal/timeline"
)

// cacheFile is the on-disk envelope: validation metadata plus the index body.
type cacheFile struct {
	Version    int    `json:"version"`     // index schema version
	InputSHA   string `json:"input_sha256"` // hash the index was built from
	ContentSHA string `json:"content_sha256"`
	Index      *Index `json:"index"`
}

// Cache persists and validates scan results under a root directory.
type Cache struct {
	root string
}

// NewCache opens (or lazily creates) a cache rooted at root.
func NewCache(root string) (*Cache, error) {
	if err := EnsureDir(root); err != nil {
		return nil, err
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, timeline.GlobalError("index.cache", false, err)
	}
	return &Cache{root: root}, nil
}

func (c *Cache) path() (string, error) {
	return safeJoin(c.root, "index.json")
}

// Save atomically writes the index via a temp file plus rename. A mismatch
// between the recorded input hash and idx.InputSHA is rejected up front.
func (c *Cache) Save(idx *Index, inputSHA string) error {
	if idx == nil {
		return timeline.GlobalError("index.cache", false, fmt.Errorf("nil index"))
	}
	if idx.InputSHA != inputSHA {
		return timeline.GlobalError("index.cache", false,
			fmt.Errorf("%w: index input hash mismatch", timeline.ErrCorruptIndex))
	}
	if idx.Version != IndexVersion {
		return timeline.GlobalError("index.cache", false,
			fmt.Errorf("%w: version %d != %d", timeline.ErrCorruptIndex, idx.Version, IndexVersion))
	}
	env := cacheFile{
		Version:    idx.Version,
		InputSHA:   inputSHA,
		ContentSHA: idx.ContentHash(),
		Index:      idx,
	}
	data, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return timeline.GlobalError("index.cache", false, err)
	}
	path, err := c.path()
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return timeline.GlobalError("index.cache", false, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return timeline.GlobalError("index.cache", false, err)
	}
	return nil
}

// Load returns a cached index only when both the schema version and the input
// SHA-256 match; any tampering or version drift yields ErrCorruptIndex.
func (c *Cache) Load(inputSHA string) (*Index, error) {
	path, err := c.path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil // cache miss is not an error
	}
	if err != nil {
		return nil, timeline.GlobalError("index.cache", false, err)
	}
	var env cacheFile
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, timeline.GlobalError("index.cache", true,
			fmt.Errorf("%w: %v", timeline.ErrCorruptIndex, err))
	}
	if env.Version != IndexVersion || env.Index == nil || env.Index.Version != IndexVersion {
		return nil, timeline.GlobalError("index.cache", true,
			fmt.Errorf("%w: stale version", timeline.ErrCorruptIndex))
	}
	if env.InputSHA != inputSHA || env.Index.InputSHA != inputSHA {
		return nil, timeline.GlobalError("index.cache", true,
			fmt.Errorf("%w: input hash changed", timeline.ErrCorruptIndex))
	}
	if env.ContentSHA != env.Index.ContentHash() {
		return nil, timeline.GlobalError("index.cache", true,
			fmt.Errorf("%w: content checksum mismatch", timeline.ErrCorruptIndex))
	}
	return env.Index, nil
}

// GetOrScan returns the cached index when valid, otherwise scans clip, persists
// it and returns the fresh index. A stale or hash-mismatched cache is treated
// as invalidation (rebuilt), while genuine read/unmarshal failures propagate.
func (c *Cache) GetOrScan(clip *fixture.Clip, inputSHA string) (idx *Index, rebuilt bool, err error) {
	cached, lerr := c.Load(inputSHA)
	if lerr != nil {
		if !errors.Is(lerr, timeline.ErrCorruptIndex) {
			return nil, false, lerr
		}
		if ierr := c.Invalidate(); ierr != nil {
			return nil, false, ierr
		}
	} else if cached != nil {
		return cached, false, nil
	}
	idx, err = Scan(clip, inputSHA)
	if err != nil {
		return nil, false, err
	}
	if err := c.Save(idx, inputSHA); err != nil {
		return nil, false, err
	}
	return idx, true, nil
}

// Invalidate removes the cache file if present (used after corruption).
func (c *Cache) Invalidate() error {
	path, err := c.path()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return timeline.GlobalError("index.cache", false, err)
	}
	return nil
}
