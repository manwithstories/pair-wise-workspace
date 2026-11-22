// Package batch coordinates chunked scanning: it persists checkpoints that
// record the input digest, media-time range and completed chunk numbers,
// enabling interruption recovery, index rebuild and digest-mismatch
// rejection. Checkpoints are written through a temp file and atomically
// renamed, and a failed validation never advances the checkpoint.
package batch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Checkpoint is the persisted resumption state.
type Checkpoint struct {
	InputDigest  string `json:"input_digest"`
	ParamsDigest string `json:"params_digest"`
	FirstTick    int64  `json:"first_tick"`
	LastTick     int64  `json:"last_tick"`
	ChunkFrames  int    `json:"chunk_frames"`
	TotalChunks  int    `json:"total_chunks"`
	// Completed is the set of chunk numbers that passed validation and
	// were atomically committed. Failed chunks never appear here.
	Completed []int `json:"completed"`
	// Failed records chunks whose validation failed. Their index row may
	// exist, but the checkpoint does not treat them as completed, so the
	// defect is never silently accepted as finished work.
	Failed []int `json:"failed"`
}

// ErrDigestMismatch means the input or parameters changed under a resume.
var ErrDigestMismatch = errors.New("batch: input or params digest mismatch, resume rejected")

// ErrInconsistent means checkpoint and index disagree on completed chunks.
var ErrInconsistent = errors.New("batch: checkpoint/index inconsistency")

const fileName = "checkpoint.json"

// Manager owns the checkpoint file inside a run directory.
type Manager struct {
	path string
	cp   Checkpoint
}

// NewManager prepares a checkpoint manager; on resume=true an existing
// checkpoint is loaded and its digests are verified, otherwise a fresh
// checkpoint is initialised.
func NewManager(dir, inputDigest, paramsDigest string, totalChunks, chunkFrames int, resume bool) (*Manager, error) {
	if totalChunks <= 0 || chunkFrames <= 0 {
		return nil, fmt.Errorf("batch: invalid chunking %d/%d", totalChunks, chunkFrames)
	}
	m := &Manager{path: filepath.Join(dir, fileName)}
	if !resume {
		m.cp = Checkpoint{
			InputDigest: inputDigest, ParamsDigest: paramsDigest,
			FirstTick: -1, ChunkFrames: chunkFrames, TotalChunks: totalChunks,
		}
		return m, m.persist()
	}
	data, err := os.ReadFile(m.path)
	if err != nil {
		return nil, fmt.Errorf("batch: resume requested but no checkpoint: %w", err)
	}
	if err := json.Unmarshal(data, &m.cp); err != nil {
		return nil, fmt.Errorf("batch: unreadable checkpoint: %w", err)
	}
	if m.cp.InputDigest != inputDigest || m.cp.ParamsDigest != paramsDigest {
		return nil, fmt.Errorf("%w: stored=%s/%s got=%s/%s",
			ErrDigestMismatch, m.cp.InputDigest, m.cp.ParamsDigest, inputDigest, paramsDigest)
	}
	if m.cp.TotalChunks != totalChunks || m.cp.ChunkFrames != chunkFrames {
		return nil, fmt.Errorf("%w: chunk layout changed", ErrDigestMismatch)
	}
	return m, nil
}

// persist writes the checkpoint via a temp file plus atomic rename.
func (m *Manager) persist() error {
	data, err := json.MarshalIndent(m.cp, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, m.path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// MarkChunked records a successfully validated, committed chunk and
// extends the media range. It refuses to mark a chunk already completed
// or one that previously failed validation.
func (m *Manager) MarkChunked(chunk int, startTick, endTick int64) error {
	if m.contains(m.cp.Failed, chunk) {
		return fmt.Errorf("batch: chunk %d previously failed, cannot complete", chunk)
	}
	if m.contains(m.cp.Completed, chunk) {
		return fmt.Errorf("batch: chunk %d already checkpointed", chunk)
	}
	if chunk < 0 || chunk >= m.cp.TotalChunks {
		return fmt.Errorf("batch: chunk %d outside [0,%d)", chunk, m.cp.TotalChunks)
	}
	if endTick < startTick {
		return fmt.Errorf("batch: inverted media range [%d,%d)", startTick, endTick)
	}
	if m.cp.FirstTick < 0 || startTick < m.cp.FirstTick {
		m.cp.FirstTick = startTick
	}
	if endTick > m.cp.LastTick {
		m.cp.LastTick = endTick
	}
	m.cp.Completed = append(m.cp.Completed, chunk)
	return m.persist()
}

// MarkFailed records a chunk whose validation failed. The checkpoint is
// persisted so the failure is durable, but the chunk is kept out of
// Completed and therefore never counts as finished work.
func (m *Manager) MarkFailed(chunk int) error {
	if chunk < 0 || chunk >= m.cp.TotalChunks {
		return fmt.Errorf("batch: chunk %d outside [0,%d)", chunk, m.cp.TotalChunks)
	}
	if m.contains(m.cp.Completed, chunk) {
		return fmt.Errorf("batch: chunk %d already completed", chunk)
	}
	if m.contains(m.cp.Failed, chunk) {
		return nil // idempotent: re-encountering a failed chunk is fine
	}
	m.cp.Failed = append(m.cp.Failed, chunk)
	return m.persist()
}

func (m *Manager) contains(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// IsFailed reports whether a chunk previously failed validation.
func (m *Manager) IsFailed(chunk int) bool { return m.contains(m.cp.Failed, chunk) }

// FailedCount returns how many chunks failed validation.
func (m *Manager) FailedCount() int { return len(m.cp.Failed) }

// Done reports whether every chunk is either completed or failed, i.e.
// there is no outstanding work left to resume.
func (m *Manager) Done() bool {
	return len(m.cp.Completed)+len(m.cp.Failed) == m.cp.TotalChunks
}

// Snapshot returns a copy of the checkpoint state.
func (m *Manager) Snapshot() Checkpoint {
	cp := m.cp
	cp.Completed = append([]int(nil), m.cp.Completed...)
	cp.Failed = append([]int(nil), m.cp.Failed...)
	return cp
}

// VerifyAgainst checks that every checkpointed chunk exists in the
// committed index set. Index rows belonging to failed chunks are allowed
// to exist without being marked completed.
func (m *Manager) VerifyAgainst(committed map[int]bool) error {
	for _, c := range m.cp.Completed {
		if !committed[c] {
			return fmt.Errorf("%w: completed chunk %d absent from index", ErrInconsistent, c)
		}
	}
	for c := range committed {
		if !m.contains(m.cp.Completed, c) && !m.contains(m.cp.Failed, c) {
			return fmt.Errorf("%w: index chunk %d absent from checkpoint", ErrInconsistent, c)
		}
	}
	return nil
}

// Digest computes a stable hex SHA-256 over the concatenation of inputs.
func Digest(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
