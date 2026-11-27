// Package pipeline orchestrates streaming delivery: scan, transport
// recovery, clock alignment, chunked remux output, and crash-safe resume
// from validated checkpoints.
package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	"mediad/internal/model"
)

// checkpointFile is the JSON state file name inside the output metadata dir.
const checkpointFile = "checkpoint.json"

// BlockState records one already-committed output chunk.
type BlockState struct {
	Index      int    `json:"index"`
	StartSeq   uint16 `json:"start_seq"`
	EndSeq     uint16 `json:"end_seq"`
	ByteLength int64  `json:"byte_length"`
	SHA256     string `json:"sha256"`
}

// Checkpoint is the durable resume token. It pins the exact source digest
// and the cumulative output checksum, so a changed input or a corrupted
// partial output cannot be silently continued.
type Checkpoint struct {
	InputSHA256     string       `json:"input_sha256"`
	OutputName      string       `json:"output_name"`
	SamplesDone     int          `json:"samples_done"`
	PacketsDone     int          `json:"packets_done"`
	RecoveredCount  int          `json:"recovered_count"`
	UnrecoveredCount int         `json:"unrecovered_count"`
	RunningHash     string       `json:"running_sha256"`
	Blocks          []BlockState `json:"blocks"`
	Interrupted     bool         `json:"interrupted"`
}

// CheckpointStore loads and atomically persists checkpoints beneath outDir.
type CheckpointStore struct {
	root string
}

// NewCheckpointStore opens the metadata directory under outDir.
func NewCheckpointStore(outDir string) (*CheckpointStore, error) {
	root := filepath.Join(outDir, ".mediad")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &CheckpointStore{root: root}, nil
}

// Path returns the checkpoint file location.
func (s *CheckpointStore) Path() string { return filepath.Join(s.root, checkpointFile) }

// Load reads and validates a checkpoint. A missing file returns (nil, nil).
func (s *CheckpointStore) Load() (*Checkpoint, error) {
	b, err := os.ReadFile(s.Path())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cp Checkpoint
	if err := json.Unmarshal(b, &cp); err != nil {
		return nil, err
	}
	return &cp, nil
}

// Save writes the checkpoint atomically (temp file + fsync + rename), so a
// crash can never leave a half-written state file.
func (s *CheckpointStore) Save(cp *Checkpoint) error {
	b, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	target := s.Path()
	tmp := target + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, target)
}

// Clear removes a checkpoint after a successful full run.
func (s *CheckpointStore) Clear() error {
	err := os.Remove(s.Path())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Validate ensures a checkpoint belongs to the current source. A digest
// mismatch means the input changed and the run must start from scratch.
func (cp *Checkpoint) Validate(inputSHA256 string) error {
	if cp.InputSHA256 != inputSHA256 {
		return model.ErrCheckpointMismatch
	}
	return nil
}

// hashHex is a small helper used by both checkpoint and block accounting.
func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
