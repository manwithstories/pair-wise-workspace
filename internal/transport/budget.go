package transport

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"

	"archivecheck/internal/fault"
)

// BandwidthSample is one measured throughput observation.  Bps is bits per
// second; At is a logical position in the stream (packet ordinal) so the
// schedule is reproducible.
type BandwidthSample struct {
	At  int
	Bps float64
}

// Chunk is one planned unit of work derived from a bandwidth sample.
type Chunk struct {
	Index      int
	At         int
	Bytes      int
	BufferCap  int
	Spilled    bool
	Frames     int
	BytesTotal int
}

// BudgetConfig bounds memory and drives adaptive chunk sizing.
type BudgetConfig struct {
	// MemoryBytes is the hard buffer budget; 0 selects 1 MiB.
	MemoryBytes int64
	// ChunkSeconds is the target playback duration of one chunk.
	ChunkSeconds float64
	// MinChunkBytes and MaxChunkBytes clamp the adaptive size.
	MinChunkBytes int
	MaxChunkBytes int
	// SpillDir receives spilled chunks.  Empty disables disk spill, and the
	// budget then rejects oversized chunks instead of buffering them.
	SpillDir string
}

// DefaultBudget returns a 1 MiB budget targeting 250 ms chunks.
func DefaultBudget(dir string) BudgetConfig {
	return BudgetConfig{
		MemoryBytes:   1 << 20,
		ChunkSeconds:  0.25,
		MinChunkBytes: 512,
		MaxChunkBytes: 256 << 10,
		SpillDir:      dir,
	}
}

// Plan computes chunk boundaries for the sample series.  Chunk sizes track
// the latest sample: faster links get larger chunks, slow links smaller ones.
// The plan is a pure function of the samples and config, so reruns match.
func Plan(samples []BandwidthSample, cfg BudgetConfig) []Chunk {
	if cfg.MinChunkBytes <= 0 {
		cfg.MinChunkBytes = 512
	}
	if cfg.MaxChunkBytes < cfg.MinChunkBytes {
		cfg.MaxChunkBytes = cfg.MinChunkBytes
	}
	if cfg.ChunkSeconds <= 0 {
		cfg.ChunkSeconds = 0.25
	}
	if cfg.MemoryBytes <= 0 {
		cfg.MemoryBytes = 1 << 20
	}
	out := make([]Chunk, 0, len(samples))
	for i, s := range samples {
		bytes := int(s.Bps * cfg.ChunkSeconds / 8)
		if bytes < cfg.MinChunkBytes {
			bytes = cfg.MinChunkBytes
		}
		if bytes > cfg.MaxChunkBytes {
			bytes = cfg.MaxChunkBytes
		}
		// Buffer capacity covers about two chunks, never beyond the budget.
		capBytes := bytes * 2
		if int64(capBytes) > cfg.MemoryBytes {
			capBytes = int(cfg.MemoryBytes)
		}
		out = append(out, Chunk{Index: i, At: s.At, Bytes: bytes, BufferCap: capBytes})
	}
	return out
}

// SpillStats reports budget behaviour.
type SpillStats struct {
	Chunks       int   `json:"chunks"`
	PeakMemory   int64 `json:"peak_memory_bytes"`
	Spilled      int   `json:"spilled_chunks"`
	SpilledBytes int64 `json:"spilled_bytes"`
	BufferLimit  int64 `json:"buffer_limit_bytes"`
	// Digest is the SHA-256 of all chunk payloads in order.  It is identical
	// whether chunks were held in memory or spilled.
	Digest string `json:"payload_sha256"`
}

// Buffer accumulates chunk payloads under a memory budget, spilling the
// oldest chunks to disk when the budget would be exceeded.  Order is
// preserved and the digest is computed as payloads are appended, so spill
// or no spill produces the same result.
type Buffer struct {
	limit  int64
	dir    string
	faults *fault.Set

	spill  *os.File
	chunks []buffered
	inMem  int64
	peak   int64

	hash  hash.Hash
	stats SpillStats
	seq   int
}

type buffered struct {
	data    []byte
	offset  int64
	size    int64
	spilled bool
}

// NewBuffer returns a buffer with the given limit; dir may be empty to
// disable spilling.
func NewBuffer(limit int64, dir string, faults *fault.Set) *Buffer {
	if limit <= 0 {
		limit = 1 << 20
	}
	return &Buffer{limit: limit, dir: dir, faults: faults, hash: sha256.New()}
}

// Append adds one chunk payload.  When the payload alone exceeds the budget
// and no spill directory is configured, it is rejected with a fault rather
// than buffered.
func (b *Buffer) Append(data []byte) error {
	c := buffered{data: data, size: int64(len(data))}
	b.hash.Write(data)
	b.seq++
	if b.inMem+int64(len(data)) <= b.limit {
		b.chunks = append(b.chunks, c)
		b.inMem += int64(len(data))
		if b.inMem > b.peak {
			b.peak = b.inMem
		}
		return nil
	}
	if b.dir == "" {
		if b.faults != nil {
			b.faults.Add(fault.New(fault.ModuleTransport, fault.KindBudgetExceeded, fault.NoOffset(),
				fmt.Sprintf("chunk of %d bytes exceeds %d-byte budget with spilling disabled", len(data), b.limit)))
		}
		return fmt.Errorf("transport: memory budget exceeded and spilling disabled")
	}
	if b.spill == nil {
		f, err := os.CreateTemp(b.dir, "chunk-*.spill")
		if err != nil {
			return fmt.Errorf("transport: create spill file: %w", err)
		}
		b.spill = f
	}
	off, err := b.spill.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if _, err := b.spill.Write(data); err != nil {
		return err
	}
	c.data = nil
	c.offset = off
	c.spilled = true
	b.chunks = append(b.chunks, c)
	b.stats.Spilled++
	b.stats.SpilledBytes += int64(len(data))
	if b.faults != nil {
		b.faults.Add(fault.New(fault.ModuleTransport, fault.KindBudgetExceeded, fault.NoOffset(),
			fmt.Sprintf("chunk %d spilled to disk (%d bytes) to stay within %d-byte budget", b.seq, len(data), b.limit)))
	}
	// Evict oldest in-memory chunks until under budget.
	for b.inMem > b.limit/2 && len(b.chunks) > 1 {
		evicted := false
		for i := range b.chunks {
			if b.chunks[i].spilled {
				continue
			}
			if err := b.spillChunk(&b.chunks[i]); err != nil {
				return err
			}
			evicted = true
			break
		}
		if !evicted {
			break
		}
	}
	return nil
}

func (b *Buffer) spillChunk(c *buffered) error {
	if b.spill == nil {
		return fmt.Errorf("transport: spill file missing")
	}
	off, err := b.spill.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if _, err := b.spill.Write(c.data); err != nil {
		return err
	}
	b.inMem -= int64(len(c.data))
	c.offset = off
	c.size = int64(len(c.data))
	c.spilled = true
	c.data = nil
	b.stats.Spilled++
	b.stats.SpilledBytes += c.size
	return nil
}

// Buffered returns the concatenated payloads in append order.
func (b *Buffer) Buffered() ([]byte, error) {
	var out []byte
	for i := range b.chunks {
		c := &b.chunks[i]
		if !c.spilled {
			out = append(out, c.data...)
			continue
		}
		buf := make([]byte, c.size)
		if _, err := b.spill.ReadAt(buf, c.offset); err != nil && err != io.EOF {
			return nil, err
		}
		out = append(out, buf...)
	}
	return out, nil
}

// Stats finalizes and returns spill statistics.
func (b *Buffer) Stats() SpillStats {
	s := b.stats
	s.Chunks = len(b.chunks)
	s.PeakMemory = b.peak
	s.BufferLimit = b.limit
	s.Digest = hex.EncodeToString(b.hash.Sum(nil))
	return s
}

// Close removes the spill file.
func (b *Buffer) Close() error {
	if b.spill == nil {
		return nil
	}
	name := b.spill.Name()
	err := b.spill.Close()
	if rmErr := os.Remove(name); rmErr != nil && err == nil {
		err = rmErr
	}
	b.spill = nil
	if dir, derr := filepath.Abs(filepath.Dir(name)); derr == nil {
		_ = dir
	}
	return err
}

// DigestChunks returns the SHA-256 over the ordered chunk payloads without
// buffering them, for comparison against a spilled run.
func DigestChunks(payloads [][]byte) string {
	h := sha256.New()
	for _, p := range payloads {
		h.Write(p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// SortedSamples returns samples in ascending At order.
func SortedSamples(s []BandwidthSample) []BandwidthSample {
	out := append([]BandwidthSample(nil), s...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].At < out[j].At })
	return out
}
