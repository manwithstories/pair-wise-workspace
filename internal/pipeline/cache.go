// Package pipeline orchestrates incremental media processing with a
// content-addressed cache and crash-safe checkpoints.
package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Key identifies a cached segment rendering. It binds the source block
// digest, the track set, the timebase and the processing parameters.
type Key struct {
	SourceSHA string
	Tracks    string
	Timebase  string
	ParamsSHA string
}

func (k Key) id() string {
	h := sha256.Sum256([]byte(k.SourceSHA + "|" + k.Tracks + "|" + k.Timebase + "|" + k.ParamsSHA))
	return hex.EncodeToString(h[:])
}

// Entry is the cached metadata for one segment.
type Entry struct {
	SourceSHA string   `json:"source_sha"`
	ParamsSHA string   `json:"params_sha"`
	FrameSHAs []string `json:"frame_shas"`
}

// Cache is a content-addressed store rooted at Dir.
type Cache struct {
	Dir string
}

// FileName exposes the metadata path so fixtures and tests can plant
// entries (including wrong-source ones).
func (c *Cache) FileName(k Key) string {
	return filepath.Join(c.Dir, "seg_"+k.id()+".json")
}

func (c *Cache) framePath(k Key, i int) string {
	return filepath.Join(c.Dir, fmt.Sprintf("seg_%s_%d.raw", k.id(), i))
}

func shaHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Get looks up an entry. The stored source identity and parameter
// digest are always re-verified; a mismatch discards the cache entry
// and reports a miss so the caller recomputes.
func (c *Cache) Get(k Key) (*Entry, [][]byte, bool) {
	meta, err := os.ReadFile(c.FileName(k))
	if err != nil {
		return nil, nil, false
	}
	var e Entry
	if err := json.Unmarshal(meta, &e); err != nil {
		c.discard(k, len(e.FrameSHAs))
		return nil, nil, false
	}
	if e.SourceSHA != k.SourceSHA || e.ParamsSHA != k.ParamsSHA {
		c.discard(k, len(e.FrameSHAs))
		return nil, nil, false
	}
	frames := make([][]byte, 0, len(e.FrameSHAs))
	for i, want := range e.FrameSHAs {
		data, err := os.ReadFile(c.framePath(k, i))
		if err != nil || shaHex(data) != want {
			c.discard(k, len(e.FrameSHAs))
			return nil, nil, false
		}
		frames = append(frames, data)
	}
	return &e, frames, true
}

// Put stores frames under the key. An existing entry from a different
// source is never overwritten blindly: it is discarded first.
func (c *Cache) Put(k Key, frames [][]byte) error {
	if err := os.MkdirAll(c.Dir, 0o755); err != nil {
		return err
	}
	if meta, err := os.ReadFile(c.FileName(k)); err == nil {
		var e Entry
		if json.Unmarshal(meta, &e) != nil || e.SourceSHA != k.SourceSHA {
			c.discard(k, len(e.FrameSHAs))
		}
	}
	e := Entry{SourceSHA: k.SourceSHA, ParamsSHA: k.ParamsSHA}
	for i, f := range frames {
		e.FrameSHAs = append(e.FrameSHAs, shaHex(f))
		if err := writeFileAtomic(c.framePath(k, i), f); err != nil {
			return err
		}
	}
	meta, err := json.Marshal(&e)
	if err != nil {
		return err
	}
	return writeFileAtomic(c.FileName(k), meta)
}

func (c *Cache) discard(k Key, nframes int) {
	_ = os.Remove(c.FileName(k))
	for i := 0; i < nframes; i++ {
		_ = os.Remove(c.framePath(k, i))
	}
}

// writeFileAtomic writes via a temp file plus rename so a crash never
// leaves a half-written artifact.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
