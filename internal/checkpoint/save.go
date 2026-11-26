package checkpoint

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/relayteam/relay/internal/media"
	"github.com/relayteam/relay/internal/timeline"
	"github.com/relayteam/relay/internal/verify"
)

// Save atomically writes checkpoint/index.json.
func (m *Manager) Save() error {
	dir := filepath.Join(m.root, "checkpoint")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return media.E(media.ErrCheckpoint, "checkpoint.save", media.Meta{}, err)
	}
	final := filepath.Join(dir, "index.json")
	tmp, err := os.CreateTemp(dir, ".index-*.tmp")
	if err != nil {
		return media.E(media.ErrCheckpoint, "checkpoint.save", media.Meta{}, err)
	}
	tmpName := tmp.Name()
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m.Index); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return media.E(media.ErrCheckpoint, "checkpoint.save", media.Meta{}, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return media.E(media.ErrCheckpoint, "checkpoint.save", media.Meta{}, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return media.E(media.ErrCheckpoint, "checkpoint.save", media.Meta{}, err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		_ = os.Remove(tmpName)
		return media.E(media.ErrCheckpoint, "checkpoint.save", media.Meta{}, err)
	}
	syncDir(dir)
	return nil
}

// CommitChunk persists records as NDJSON, verifies the bytes just written,
// and only then appends and atomically commits the index entry.
func (m *Manager) CommitChunk(records []timeline.Record) (Chunk, error) {
	n := len(m.Index.Chunks)
	rel := fmt.Sprintf("%s/chunk-%06d.ndjson", chunkDir, n)
	path := filepath.Join(m.root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Chunk{}, media.E(media.ErrCheckpoint, "checkpoint.chunk", media.Meta{}, err)
	}
	f, err := os.Create(path)
	if err != nil {
		return Chunk{}, media.E(media.ErrCheckpoint, "checkpoint.chunk", media.Meta{}, err)
	}
	h := newChunkHasher(f)
	enc := json.NewEncoder(h)
	first, last := 0, 0
	for i := range records {
		if i == 0 {
			first = records[i].RecordNo
		}
		last = records[i].RecordNo
		if err := enc.Encode(&records[i]); err != nil {
			_ = f.Close()
			return Chunk{}, media.E(media.ErrCheckpoint, "checkpoint.chunk",
				records[i].Meta(records[i].RecordNo, records[i].LineNo), err)
		}
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return Chunk{}, media.E(media.ErrCheckpoint, "checkpoint.chunk", media.Meta{}, err)
	}
	if err := f.Close(); err != nil {
		return Chunk{}, media.E(media.ErrCheckpoint, "checkpoint.chunk", media.Meta{}, err)
	}
	sum, crc, size := h.digest()
	c := Chunk{
		Index: n, File: rel, RecordCount: len(records), FirstRecord: first,
		LastRecord: last, ByteCount: size, SHA256: sum, CRC32: crc,
	}
	if err := verifyChunkFile(m.root, c); err != nil {
		return Chunk{}, err
	}
	m.Index.Chunks = append(m.Index.Chunks, c)
	if last > 0 {
		m.Index.NextRecordNo = last + 1
	}
	if err := m.Save(); err != nil {
		return Chunk{}, err
	}
	return c, nil
}

func verifyChunkFile(root string, c Chunk) error {
	path := filepath.Join(root, c.File)
	sum, err := verify.SHA256File(path)
	if err != nil {
		return media.E(media.ErrChunkDigest, "checkpoint.verify", media.Meta{}, err)
	}
	crc, err := verify.CRC32File(path)
	if err != nil {
		return media.E(media.ErrChunkDigest, "checkpoint.verify", media.Meta{}, err)
	}
	if sum != c.SHA256 || crc != c.CRC32 {
		return media.E(media.ErrChunkDigest, "checkpoint.verify", media.Meta{}, fmt.Errorf(
			"chunk %d digest mismatch", c.Index))
	}
	return nil
}

func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
