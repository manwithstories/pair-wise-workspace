// Package checkpoint persists resumable recovery state atomically and pins it
// to the input digest so a resumed run can prove it is continuing the same
// input.
package checkpoint

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ErrMismatch reports a checkpoint that belongs to different input.
var ErrMismatch = errors.New("checkpoint input digest mismatch")

// ErrCorrupt reports an unreadable or self-inconsistent checkpoint.
var ErrCorrupt = errors.New("checkpoint corrupt")

// Format is the on-disk schema version.
const Format = 1

// State is everything needed to resume without redoing completed work.
type State struct {
	Format int `json:"format"`
	// InputSHA256 pins the run to one exact input manifest.
	InputSHA256 string `json:"input_sha256"`
	// Profile echoes the fixture profile for diagnostics.
	Profile string `json:"profile"`
	// CompletedRepresentations lists representation names fully recovered.
	CompletedRepresentations []string `json:"completed_representations"`
	// Inflight is the representation interrupted mid-stream, if any.
	Inflight string `json:"inflight,omitempty"`
	// InflightSegments is how many fragments of Inflight were committed.
	InflightSegments int `json:"inflight_segments,omitempty"`
	// InterruptAt is the fragment count at which --interrupt-after fires.
	InterruptAt int `json:"interrupt_at,omitempty"`
	// StartedAt is a wall-clock stamp for resume_ms reporting.
	StartedAt time.Time `json:"started_at"`
	// WrittenAt is when this state was last flushed.
	WrittenAt time.Time `json:"written_at"`
	// TimelineSHA256 is set when all representations and fan-in completed.
	TimelineSHA256 string `json:"timeline_sha256,omitempty"`
	// FrameSHA256 is set when the export finished.
	FrameSHA256 string `json:"frame_sha256,omitempty"`
	// FrameCount is the exported frame count.
	FrameCount int `json:"frame_count,omitempty"`
	// ResumeCount counts how many times this state resumed an interruption.
	ResumeCount int `json:"resume_count,omitempty"`
}

// Manager owns the checkpoint file lifecycle.
type Manager struct {
	path string
}

// Open prepares a checkpoint manager. The file need not exist yet.
func Open(path string) (*Manager, error) {
	if path == "" {
		return nil, errors.New("checkpoint path required")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("checkpoint dir: %w", err)
	}
	return &Manager{path: path}, nil
}

// Load reads and validates the checkpoint against inputSHA256.
// A missing file returns (nil, nil).
func (m *Manager) Load(inputSHA256 string) (*State, error) {
	data, err := os.ReadFile(m.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read checkpoint: %w", err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if st.Format != Format {
		return nil, fmt.Errorf("%w: format %d unsupported", ErrCorrupt, st.Format)
	}
	if st.InputSHA256 != inputSHA256 {
		return nil, fmt.Errorf("%w: have %s want %s", ErrMismatch, st.InputSHA256, inputSHA256)
	}
	return &st, nil
}

// Save writes state atomically: full temp file, fsync, rename, then dir fsync.
// A torn checkpoint can therefore never be observed.
func (m *Manager) Save(st *State) error {
	st.Format = Format
	st.WrittenAt = time.Now().UTC()
	payload, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal checkpoint: %w", err)
	}
	payload = append(payload, '\n')
	return AtomicWriteFile(m.path, payload, 0o644)
}

// AtomicWriteFile writes data to path via a sibling temp file and rename.
func AtomicWriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp: %w", err)
	}
	tmpName = ""
	// Fsync the directory so the rename is durable.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
