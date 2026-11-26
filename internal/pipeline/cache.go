package pipeline

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"

	"mediad/internal/bmff"
	"mediad/internal/model"
)

const MaxIndexCacheBytes = 64 << 20

type indexCacheFile struct {
	Version     int         `json:"version"`
	InputSHA256 string      `json:"input_sha256"`
	Index       *bmff.Index `json:"index"`
}

type CachedIndex struct {
	Index *bmff.Index
	State string // rebuilt | hit | invalidated
}

// LoadOrBuildIndex validates the cache against a full-input SHA-256. Missing
// caches are rebuilt; truncation, JSON corruption, version changes and input
// digest mismatches all invalidate and transparently rebuild.
func LoadOrBuildIndex(inputPath, cachePath string) (*CachedIndex, error) {
	inputSHA, err := HashFileSHA256(inputPath)
	if err != nil {
		return nil, err
	}
	if cachePath != "" {
		if cached, state, err := readIndexCache(cachePath, inputSHA); err != nil {
			return nil, err
		} else if cached != nil {
			return &CachedIndex{Index: cached, State: state}, nil
		} else if state == "invalidated" {
			idx, err := bmff.ScanFile(inputPath)
			if err != nil {
				return nil, err
			}
			if err := writeIndexCache(cachePath, inputSHA, idx); err != nil {
				return nil, err
			}
			return &CachedIndex{Index: idx, State: "invalidated"}, nil
		}
	}
	idx, err := bmff.ScanFile(inputPath)
	if err != nil {
		return nil, err
	}
	if cachePath != "" {
		if err := writeIndexCache(cachePath, inputSHA, idx); err != nil {
			return nil, err
		}
	}
	return &CachedIndex{Index: idx, State: "rebuilt"}, nil
}

func readIndexCache(path, inputSHA string) (*bmff.Index, string, error) {
	st, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, "rebuilt", nil
		}
		return nil, "", model.Wrap(model.CodeInternal, model.StageIndex, "stat index cache", err).WithSource(path)
	}
	if st.Size() > MaxIndexCacheBytes {
		return nil, "invalidated", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, "invalidated", nil
	}
	var cache indexCacheFile
	if err := json.Unmarshal(b, &cache); err != nil {
		return nil, "invalidated", nil
	}
	if cache.Version != bmff.IndexVersion || cache.Index == nil || cache.Index.Version != bmff.IndexVersion {
		return nil, "invalidated", nil
	}
	if cache.InputSHA256 != inputSHA || cache.Index.InputSHA256 != inputSHA {
		return nil, "invalidated", nil
	}
	return cache.Index, "hit", nil
}

func writeIndexCache(path, inputSHA string, idx *bmff.Index) error {
	cache := indexCacheFile{Version: bmff.IndexVersion, InputSHA256: inputSHA, Index: idx}
	b, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return model.Wrap(model.CodeInternal, model.StageIndex, "encode index cache", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return model.Wrap(model.CodeInternal, model.StageIndex, "write index cache", err).WithSource(path)
	}
	if err := os.Rename(tmp, path); err != nil {
		return model.Wrap(model.CodeInternal, model.StageIndex, "commit index cache", err).WithSource(path)
	}
	return nil
}

// InvalidateIndexForTest writes a valid-enough stale cache marker. It is also
// useful for operational cache resets.
func InvalidateIndexForTest(cachePath string) error {
	return os.WriteFile(cachePath, []byte(`{"version":-1}`), 0o644)
}
