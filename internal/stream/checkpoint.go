package stream

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"relay/internal/media"
)

// CheckpointEntry records one durably committed fragment. The source hash
// keys the entry so a resumed run skips exactly the committed work and a
// changed input fragment is reprocessed rather than silently reused.
type CheckpointEntry struct {
	Key        string `json:"key"`
	TrackID    uint32 `json:"track_id"`
	Sequence   uint32 `json:"sequence"`
	SourceHash string `json:"source_hash"`
	OutputHash string `json:"output_hash"`
	OutputSize int64  `json:"output_size"`
}

// Checkpoint is an append-only, fsync-backed commit log.
type Checkpoint struct {
	mu       sync.Mutex
	path     string
	file     *os.File
	writer   *bufio.Writer
	committed map[string]CheckpointEntry
}

// OpenCheckpoint loads any existing log at dir/checkpoint.jsonl and appends
// to it. A corrupt trailing line is reported as a checkpoint fault rather
// than silently truncated.
func OpenCheckpoint(dir string) (*Checkpoint, error) {
	path := filepath.Join(dir, "checkpoint.jsonl")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, media.NewFault(media.FaultCheckpoint, 0, 0, 0,
			"cannot open checkpoint: "+err.Error())
	}
	cp := &Checkpoint{path: path, file: f, committed: map[string]CheckpointEntry{}}
	if err := cp.load(); err != nil {
		f.Close()
		return nil, err
	}
	cp.writer = bufio.NewWriter(f)
	return cp, nil
}

func (cp *Checkpoint) load() error {
	dec := json.NewDecoder(cp.file)
	for {
		var e CheckpointEntry
		if err := dec.Decode(&e); err == io.EOF {
			break
		} else if err != nil {
			return media.NewFault(media.FaultCheckpoint, 0, 0,
				dec.InputOffset(), "corrupt checkpoint record: "+err.Error())
		}
		cp.committed[e.Key] = e
	}
	if _, err := cp.file.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	return nil
}

// Committed returns the entry for a key and whether it exists.
func (cp *Checkpoint) Committed(key string) (CheckpointEntry, bool) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	e, ok := cp.committed[key]
	return e, ok
}

// Commit appends an entry, flushes and fsyncs it before returning. The
// durable order is the only thing a resumed run relies on.
func (cp *Checkpoint) Commit(e CheckpointEntry) error {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	line, err := json.Marshal(e)
	if err != nil {
		return media.NewFault(media.FaultCheckpoint, e.TrackID, e.Sequence, 0,
			"encode checkpoint: "+err.Error())
	}
	if _, err := cp.writer.Write(append(line, '\n')); err != nil {
		return media.NewFault(media.FaultCheckpoint, e.TrackID, e.Sequence, 0,
			"write checkpoint: "+err.Error())
	}
	if err := cp.writer.Flush(); err != nil {
		return media.NewFault(media.FaultCheckpoint, e.TrackID, e.Sequence, 0,
			"flush checkpoint: "+err.Error())
	}
	if err := cp.file.Sync(); err != nil {
		return media.NewFault(media.FaultCheckpoint, e.TrackID, e.Sequence, 0,
			"fsync checkpoint: "+err.Error())
	}
	cp.committed[e.Key] = e
	return nil
}

// Entries returns committed entries in deterministic key order.
func (cp *Checkpoint) Entries() []CheckpointEntry {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	out := make([]CheckpointEntry, 0, len(cp.committed))
	for _, e := range cp.committed {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Close flushes and releases the log file.
func (cp *Checkpoint) Close() error {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if err := cp.writer.Flush(); err != nil {
		return err
	}
	return cp.file.Close()
}

// HashBytes is the canonical sha256 used for source and output content.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
