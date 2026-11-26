package checkpoint

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/relayteam/relay/internal/media"
	"github.com/relayteam/relay/internal/timeline"
)

// VerifyCommitted checks every index-referenced chunk.
func (m *Manager) VerifyCommitted() error {
	for _, c := range m.Index.Chunks {
		if err := verifyChunkFile(m.root, c); err != nil {
			return err
		}
	}
	return nil
}

// CleanOrphans removes chunk files not present in the committed index.
func (m *Manager) CleanOrphans() (int, error) {
	dir := filepath.Join(m.root, chunkDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, media.E(media.ErrCheckpoint, "checkpoint.recover", media.Meta{}, err)
	}
	committed := make(map[string]bool, len(m.Index.Chunks))
	for _, c := range m.Index.Chunks {
		committed[filepath.Base(c.File)] = true
	}
	removed := 0
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".ndjson" || committed[e.Name()] {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			return removed, media.E(media.ErrCheckpoint, "checkpoint.recover", media.Meta{}, err)
		}
		removed++
	}
	return removed, nil
}

// StreamCommitted replays verified chunks in deterministic chunk order.
func (m *Manager) StreamCommitted(fn func(timeline.Record) error) error {
	chunks := append([]Chunk(nil), m.Index.Chunks...)
	sort.Slice(chunks, func(i, j int) bool { return chunks[i].Index < chunks[j].Index })
	for _, c := range chunks {
		f, err := os.Open(filepath.Join(m.root, c.File))
		if err != nil {
			return media.E(media.ErrChunkDigest, "checkpoint.replay", media.Meta{}, err)
		}
		dec := json.NewDecoder(f)
		for {
			var rec timeline.Record
			if err := dec.Decode(&rec); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				_ = f.Close()
				return media.E(media.ErrChunkDigest, "checkpoint.replay", media.Meta{}, err)
			}
			if err := fn(rec); err != nil {
				_ = f.Close()
				return err
			}
		}
		_ = f.Close()
	}
	return nil
}

// Restore performs the complete pre-resume verification sequence.
func Restore(root, inputFingerprint string, cfg Config) (*Manager, int64, error) {
	start := time.Now()
	m, err := NewManager(root, inputFingerprint, cfg)
	if err != nil {
		return nil, time.Since(start).Milliseconds(), err
	}
	if m.Index != nil {
		if err := m.VerifyCommitted(); err != nil {
			return nil, time.Since(start).Milliseconds(), err
		}
		if _, err := m.CleanOrphans(); err != nil {
			return nil, time.Since(start).Milliseconds(), err
		}
	}
	return m, time.Since(start).Milliseconds(), nil
}
