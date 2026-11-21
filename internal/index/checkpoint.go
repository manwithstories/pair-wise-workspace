package index

import (
	"crypto/sha256"
	"encoding/hex"
)

const (
	// CheckpointVersion is the on-disk schema version.
	CheckpointVersion = 1
	// DefaultCheckpointEvery matches the acceptance requirement.
	DefaultCheckpointEvery = 256
	checkpointName         = "checkpoint.json"
)

// Checkpoint is the resumable cursor persisted once per chunk.
type Checkpoint struct {
	Version          int      `json:"version"`
	InputDigest      string   `json:"input_digest"`
	ChunkEvery       int      `json:"chunk_every"`
	CommittedSamples int      `json:"committed_samples"`
	CommittedChunks  int      `json:"committed_chunks"`
	CommittedSegs    []string `json:"committed_segment_ids"`
	CursorInput      string   `json:"cursor_input"`
	CursorSample     int      `json:"cursor_sample"`
	Checksum         string   `json:"checksum"`
}

// Chunker decides when a checkpoint is due and guarantees each committed
// chunk is counted exactly once, even across resume and duplicate starts.
type Chunker struct {
	every    int
	cp       Checkpoint
	seenSegs map[string]struct{}
}

// NewChunker starts a fresh chunker for the given input digest.
func NewChunker(inputDigest string, every int) *Chunker {
	if every <= 0 {
		every = DefaultCheckpointEvery
	}
	return &Chunker{
		every: every,
		cp: Checkpoint{
			Version:     CheckpointVersion,
			InputDigest: inputDigest,
			ChunkEvery:  every,
			CommittedSegs: []string{},
		},
		seenSegs: map[string]struct{}{},
	}
}

// Observe records one processed sample. It returns true when a chunk
// boundary (every N samples) has been reached and a checkpoint is due.
func (c *Chunker) Observe(inputID string, sampleIndex int) bool {
	c.cp.CursorInput = inputID
	c.cp.CursorSample = sampleIndex + 1
	c.cp.CommittedSamples++
	return c.cp.CommittedSamples%c.every == 0
}

// CommitSegment records a segment id at most once. It reports whether the
// segment was newly counted, which is the duplicate-run idempotency guard.
func (c *Chunker) CommitSegment(id string) bool {
	if _, ok := c.seenSegs[id]; ok {
		return false
	}
	c.seenSegs[id] = struct{}{}
	c.cp.CommittedSegs = append(c.cp.CommittedSegs, id)
	return true
}

// Snapshot returns a checksummed copy ready for atomic persistence.
func (c *Chunker) Snapshot() Checkpoint {
	cp := c.cp
	cp.CommittedChunks = c.cp.CommittedSamples / c.every
	cp.Checksum = c.bodyChecksum(cp)
	return cp
}

// bodyChecksum covers every resumable field except the checksum itself, so
// tampering or partial writes are detectable on restart.
func (c *Chunker) bodyChecksum(cp Checkpoint) string {
	h := sha256.New()
	write := func(s string) { h.Write([]byte(s)) }
	write(itoa(cp.Version))
	write(cp.InputDigest)
	write(itoa(cp.ChunkEvery))
	write(itoa(cp.CommittedSamples))
	write(itoa(cp.CommittedChunks))
	write(itoa(len(cp.CommittedSegs)))
	for _, id := range cp.CommittedSegs {
		write(id)
	}
	write(cp.CursorInput)
	write(itoa(cp.CursorSample))
	return hex.EncodeToString(h.Sum(nil))
}

// Save atomically writes the current checkpoint through the store.
func (c *Chunker) Save(s *Store) error {
	return s.AtomicWrite(checkpointName, c.Snapshot())
}

// LoadCheckpoint reads and validates a persisted checkpoint. A digest
// mismatch with the current input refuses resume rather than mixing two
// inputs; ErrNotFound means this is a clean first run.
func LoadCheckpoint(s *Store, currentInputDigest string) (*Checkpoint, error) {
	var cp Checkpoint
	if err := s.ReadJSON(checkpointName, &cp); err != nil {
		return nil, err
	}
	if cp.Version != CheckpointVersion {
		return nil, ErrChecksum
	}
	want := (&Chunker{every: cp.ChunkEvery}).bodyChecksum(cp)
	if want != cp.Checksum {
		return nil, ErrChecksum
	}
	if cp.InputDigest != currentInputDigest {
		return nil, ErrDigest
	}
	return &cp, nil
}

// Resume rebuilds a chunker from a validated checkpoint so already
// committed chunks are not counted again.
func Resume(cp Checkpoint) *Chunker {
	c := NewChunker(cp.InputDigest, cp.ChunkEvery)
	c.cp.CommittedSamples = cp.CommittedSamples
	c.cp.CommittedChunks = cp.CommittedChunks
	c.cp.CommittedSegs = append([]string(nil), cp.CommittedSegs...)
	c.cp.CursorInput = cp.CursorInput
	c.cp.CursorSample = cp.CursorSample
	for _, id := range c.cp.CommittedSegs {
		c.seenSegs[id] = struct{}{}
	}
	return c
}

// Samples returns the total observed sample count.
func (c *Chunker) Samples() int { return c.cp.CommittedSamples }

// Chunks returns the number of fully committed chunks.
func (c *Chunker) Chunks() int { return c.cp.CommittedSamples / c.every }

// SegmentCount returns how many distinct segments have been committed.
func (c *Chunker) SegmentCount() int { return len(c.cp.CommittedSegs) }

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
