package checkpoint

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/relayteam/relay/internal/media"
	"github.com/relayteam/relay/internal/timeline"
)

// ChunkWriter streams records directly into one chunk file. Records do not
// need to be accumulated in a memory-sized batch before the N-record commit.
type ChunkWriter struct {
	mgr          *Manager
	index        int
	relativePath string
	path         string
	file         *os.File
	hasher       *chunkHasher
	encoder      *json.Encoder
	count        int
	firstRecord  int
	lastRecord   int
}

// BeginChunk creates the next chunk file. It remains uncommitted in the index
// until Commit succeeds, so an interrupted file is safe to discard/recompute.
func (m *Manager) BeginChunk() (*ChunkWriter, error) {
	n := len(m.Index.Chunks)
	rel := fmt.Sprintf("%s/chunk-%06d.ndjson", chunkDir, n)
	path := filepath.Join(m.root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, media.E(media.ErrCheckpoint, "checkpoint.begin_chunk", media.Meta{}, err)
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, media.E(media.ErrCheckpoint, "checkpoint.begin_chunk", media.Meta{}, err)
	}
	h := newChunkHasher(f)
	return &ChunkWriter{
		mgr: m, index: n, relativePath: rel, path: path,
		file: f, hasher: h, encoder: json.NewEncoder(h),
	}, nil
}

// Write appends and hashes one record.
func (w *ChunkWriter) Write(rec timeline.Record) error {
	if w.count == 0 {
		w.firstRecord = rec.RecordNo
	}
	w.lastRecord = rec.RecordNo
	w.count++
	if err := w.encoder.Encode(&rec); err != nil {
		return media.E(media.ErrCheckpoint, "checkpoint.write_chunk",
			rec.Meta(rec.RecordNo, rec.LineNo), err)
	}
	return nil
}

// Count returns records written to the open chunk.
func (w *ChunkWriter) Count() int { return w.count }

// Persist closes/syncs bytes without adding the atomic index entry. This is
// the crash window used by the demo: the file exists but is still uncommitted.
func (w *ChunkWriter) Persist() error {
	if w.file == nil {
		return nil
	}
	if err := w.file.Sync(); err != nil {
		_ = w.file.Close()
		w.file = nil
		return media.E(media.ErrCheckpoint, "checkpoint.persist_chunk", media.Meta{}, err)
	}
	if err := w.file.Close(); err != nil {
		w.file = nil
		return media.E(media.ErrCheckpoint, "checkpoint.persist_chunk", media.Meta{}, err)
	}
	w.file = nil
	return nil
}

// Commit persists all bytes, verifies them, and atomically commits the index.
func (w *ChunkWriter) Commit() (Chunk, error) {
	if err := w.Persist(); err != nil {
		return Chunk{}, err
	}
	sum, crc, size := w.hasher.digest()
	c := Chunk{
		Index: w.index, File: w.relativePath, RecordCount: w.count,
		FirstRecord: w.firstRecord, LastRecord: w.lastRecord,
		ByteCount: size, SHA256: sum, CRC32: crc,
	}
	if err := verifyChunkFile(w.mgr.root, c); err != nil {
		return Chunk{}, err
	}
	w.mgr.Index.Chunks = append(w.mgr.Index.Chunks, c)
	if c.LastRecord > 0 {
		w.mgr.Index.NextRecordNo = c.LastRecord + 1
	}
	if err := w.mgr.Save(); err != nil {
		return Chunk{}, err
	}
	return c, nil
}
