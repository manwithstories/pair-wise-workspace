// Package checkpoint persists pipeline progress atomically. A checkpoint is
// only committed after the corresponding fragment file has been fully written
// and fsynced, so recovery can never observe a fragment whose completion was
// not recorded, and never rewrites a fragment that already exists.
package checkpoint

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"mediapack/internal/fault"
)

const fileName = "checkpoint.json"

// State is the resumable pipeline position.
type State struct {
	SourceID       string `json:"source_id"`
	InputSHA256    string `json:"input_sha256"`
	NextAU         int    `json:"next_au"`
	CompletedFrags []string `json:"completed_fragments"`
	OutputSHA256   string `json:"output_sha256"`
	Committed      bool   `json:"committed"`
}

// Manager owns the checkpoint file inside a working directory.
type Manager struct {
	dir string
}

// NewManager opens a checkpoint directory, rejecting traversal paths.
func NewManager(dir string) (*Manager, error) {
	clean := filepath.Clean(dir)
	if strings.Contains(clean, "..") {
		return nil, fault.New(fault.KindLimit, "checkpoint", "path traversal rejected")
	}
	if err := os.MkdirAll(clean, 0o755); err != nil {
		return nil, fault.Wrap(err, "checkpoint")
	}
	return &Manager{dir: clean}, nil
}

// Load returns the saved state. A missing checkpoint is not an error: the
// boolean reports whether one existed.
func (m *Manager) Load() (State, bool, error) {
	raw, err := os.ReadFile(m.path())
	if os.IsNotExist(err) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, fault.Wrap(err, "checkpoint")
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return State{}, false, fault.At(fault.KindIO, "checkpoint", -1,
			"unparseable checkpoint: "+err.Error())
	}
	return st, true, nil
}

// Commit atomically replaces the checkpoint only after the caller guarantees
// the named fragment files are durable. The directory itself is fsynced so
// the rename survives a crash immediately afterwards.
func (m *Manager) Commit(st State) error {
	st.Committed = true
	raw, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		return fault.Wrap(err, "checkpoint")
	}
	tmp, err := os.CreateTemp(m.dir, ".cp-*")
	if err != nil {
		return fault.Wrap(err, "checkpoint")
	}
	tmpName := tmp.Name()
	abort := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		abort()
		return fault.Wrap(err, "checkpoint")
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		abort()
		return fault.Wrap(err, "checkpoint")
	}
	if err := tmp.Close(); err != nil {
		abort()
		return fault.Wrap(err, "checkpoint")
	}
	if err := os.Rename(tmpName, m.path()); err != nil {
		abort()
		return fault.Wrap(err, "checkpoint")
	}
	// fsync the directory entry so the rename itself is durable.
	dir, err := os.Open(m.dir)
	if err != nil {
		return fault.Wrap(err, "checkpoint")
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fault.Wrap(err, "checkpoint")
	}
	return nil
}

// FragmentDurable writes data to name inside the working directory, fsyncs it
// and returns its size. Checkpointing is the caller's separate next step.
func (m *Manager) FragmentDurable(name string, data []byte) (int64, error) {
	if filepath.Base(name) != name || strings.Contains(name, "..") {
		return 0, fault.New(fault.KindLimit, "checkpoint",
			"fragment name must be a bare file name")
	}
	full := filepath.Join(m.dir, name)
	f, err := os.OpenFile(full, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, fault.Wrap(err, "checkpoint")
	}
	n, err := f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, fault.Wrap(err, "checkpoint")
	}
	return int64(n), nil
}

// FragmentExists reports whether a completed fragment is already on disk,
// letting recovery skip rewriting it.
func (m *Manager) FragmentExists(name string) bool {
	_, err := os.Stat(filepath.Join(m.dir, filepath.Base(name)))
	return err == nil
}

// Dir exposes the working directory for the muxer and cache integration.
func (m *Manager) Dir() string { return m.dir }

func (m *Manager) path() string { return filepath.Join(m.dir, fileName) }
