// Package task persists encode progress as a state machine with checkpoints.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// StateVersion is the on-disk state schema version.
const StateVersion = 1

// SegStatus is the lifecycle state of one segment.
type SegStatus string

// Segment lifecycle states.
const (
	Pending SegStatus = "pending"
	Running SegStatus = "running"
	Failed  SegStatus = "failed"
	Done    SegStatus = "done"
)

// ErrCorrupt marks an unreadable or inconsistent state file.
var ErrCorrupt = errors.New("state_corrupt")

// SegCheckpoint records the outcome of one segment.
type SegCheckpoint struct {
	Index  int       `json:"index"`
	Status SegStatus `json:"status"`
	QPEff  float64   `json:"qp_eff"`
	Bits   int64     `json:"bits"`
	File   string    `json:"file"`
}

// State is the top-level checkpoint document.
type State struct {
	Version      int             `json:"version"`
	ManifestPath string          `json:"manifest_path"`
	TargetKbps   int             `json:"target_kbps"`
	Segments     []SegCheckpoint `json:"segments"`
	UpdatedAt    string          `json:"updated_at"`
}

// New creates a fresh state with all segments pending.
func New(manifestPath string, targetKbps, nSegs int) *State {
	s := &State{Version: StateVersion, ManifestPath: manifestPath, TargetKbps: targetKbps}
	for i := 0; i < nSegs; i++ {
		s.Segments = append(s.Segments, SegCheckpoint{Index: i, Status: Pending})
	}
	return s
}

// Load reads and validates a state file; any inconsistency yields ErrCorrupt.
func Load(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if s.Version != StateVersion {
		return nil, fmt.Errorf("%w: unsupported version %d", ErrCorrupt, s.Version)
	}
	if len(s.Segments) == 0 {
		return nil, fmt.Errorf("%w: no segments", ErrCorrupt)
	}
	for i, seg := range s.Segments {
		if seg.Index != i {
			return nil, fmt.Errorf("%w: segment order broken at %d", ErrCorrupt, i)
		}
		switch seg.Status {
		case Pending, Running, Failed:
		case Done:
			if seg.Bits <= 0 || seg.File == "" {
				return nil, fmt.Errorf("%w: segment %d marked done without output", ErrCorrupt, i)
			}
		default:
			return nil, fmt.Errorf("%w: segment %d has unknown status %q", ErrCorrupt, i, seg.Status)
		}
	}
	return &s, nil
}

// Save atomically writes the state and appends a log-style checkpoint line.
func (s *State) Save(path string) error {
	s.UpdatedAt = time.Now().Format(time.RFC3339)
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	logLine := fmt.Sprintf("%s target_kbps=%d", s.UpdatedAt, s.TargetKbps)
	for _, seg := range s.Segments {
		logLine += fmt.Sprintf(" seg%d=%s", seg.Index, seg.Status)
	}
	logLine += "\n"
	fh, err := os.OpenFile(checkpointLogPath(path), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer fh.Close()
	_, err = fh.WriteString(logLine)
	return err
}

func checkpointLogPath(statePath string) string {
	return filepath.Join(filepath.Dir(statePath), "checkpoints.log")
}

// MarkDone records a finished segment and persists the checkpoint.
func (s *State) MarkDone(idx int, qpEff float64, bits int64, file, statePath string) error {
	s.Segments[idx] = SegCheckpoint{Index: idx, Status: Done, QPEff: qpEff, Bits: bits, File: file}
	return s.Save(statePath)
}

// PendingIndices lists segments not yet done.
func (s *State) PendingIndices() []int {
	var out []int
	for _, seg := range s.Segments {
		if seg.Status != Done {
			out = append(out, seg.Index)
		}
	}
	return out
}
