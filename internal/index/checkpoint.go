// Package index persists per-chunk checkpoints of the remux output so a
// restarted batch can verify what was committed and resume. Index writes
// are atomic (tmp file + rename). On restart the input hash, format
// version and committed chunk boundaries are verified; on corruption the
// first invalid chunk is located and processing resumes from the last
// good checkpoint.
package index

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"archive/internal/errs"
)

// Version is the checkpoint format version; mismatches refuse to resume.
const Version = 1

// Chunk records one committed output chunk.
type Chunk struct {
	Index    int    `json:"index"`
	ByteEnd  int64  `json:"byte_end"`  // output offset at chunk end
	InputPos int    `json:"input_pos"` // segments processed so far
	SHA256   string `json:"sha256"`    // hash of output bytes (prevEnd, ByteEnd]
}

// Checkpoint is the persistent index of one asset's output.
type Checkpoint struct {
	Version    int     `json:"version"`
	Asset      string  `json:"asset"`
	InputHash  string  `json:"input_hash"` // over manifest + segment content hashes
	ChunkBytes int64   `json:"chunk_bytes"`
	Chunks     []Chunk `json:"chunks"`
}

// New creates an empty checkpoint.
func New(asset, inputHash string, chunkBytes int64) *Checkpoint {
	return &Checkpoint{Version: Version, Asset: asset, InputHash: inputHash, ChunkBytes: chunkBytes}
}

// Save atomically persists the checkpoint (tmp file + rename + sync).
func Save(path string, cp *Checkpoint) error {
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return errs.New(errs.KindIndex, cp.Asset, path, "cannot write checkpoint", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return errs.New(errs.KindIndex, cp.Asset, path, "cannot write checkpoint", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return errs.New(errs.KindIndex, cp.Asset, path, "cannot sync checkpoint", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return errs.New(errs.KindIndex, cp.Asset, path, "cannot commit checkpoint", err)
	}
	return nil
}

// Load reads a checkpoint file. A missing file returns (nil, nil); a
// corrupt file returns a KindIndex error.
func Load(path string) (*Checkpoint, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, errs.New(errs.KindIndex, "", path, "cannot read checkpoint", err)
	}
	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, errs.New(errs.KindIndex, "", path, "corrupt checkpoint index", err)
	}
	return &cp, nil
}

// Verify checks the checkpoint is usable for this asset and input.
func (cp *Checkpoint) Verify(asset, inputHash string) error {
	if cp.Version != Version {
		return errs.Newf(errs.KindIndex, asset, "checkpoint", "unsupported version %d", cp.Version)
	}
	if cp.Asset != asset || cp.InputHash != inputHash {
		return errs.New(errs.KindIndex, asset, "checkpoint", "input hash changed since checkpoint", nil)
	}
	return nil
}

// FirstInvalidChunk hashes the committed chunk boundaries of r (the
// output file) and returns the number of leading valid chunks. The first
// invalid chunk — hash mismatch or truncated file — is where recovery
// starts.
func (cp *Checkpoint) FirstInvalidChunk(r io.ReaderAt, size int64) int {
	var prev int64
	for i, c := range cp.Chunks {
		if c.Index != i || c.ByteEnd <= prev || c.ByteEnd > size {
			return i // broken boundary chain or truncated output
		}
		h := sha256.New()
		if _, err := io.CopyN(h, io.NewSectionReader(r, prev, c.ByteEnd-prev), c.ByteEnd-prev); err != nil {
			return i
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != c.SHA256 {
			return i
		}
		prev = c.ByteEnd
	}
	return len(cp.Chunks)
}

// ResumePoint derives where processing resumes after n valid chunks:
// the output truncation boundary and the input position to continue from.
// n == len(Chunks) means the whole committed file is valid.
func (cp *Checkpoint) ResumePoint(validChunks int) (truncateTo int64, inputPos int) {
	if validChunks <= 0 || validChunks > len(cp.Chunks) {
		return 0, 0
	}
	c := cp.Chunks[validChunks-1]
	return c.ByteEnd, c.InputPos
}

// CoveredBytes is the output size the committed chunks vouch for.
func (cp *Checkpoint) CoveredBytes() int64 {
	if len(cp.Chunks) == 0 {
		return 0
	}
	return cp.Chunks[len(cp.Chunks)-1].ByteEnd
}

// HashRange hashes output bytes (prevEnd, end] for a new chunk record.
func HashRange(r io.ReaderAt, prevEnd, end int64) (string, error) {
	if end <= prevEnd {
		return "", fmt.Errorf("invalid chunk range (%d, %d]", prevEnd, end)
	}
	h := sha256.New()
	if _, err := io.CopyN(h, io.NewSectionReader(r, prevEnd, end-prevEnd), end-prevEnd); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
