package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Store persists per-task checkpoints and per-job fingerprints under one
// workspace-local directory.
type Store struct {
	Dir string
}

// New creates the checkpoint directory if needed.
func New(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("checkpoint directory is empty")
	}
	abs, err := filepath.Abs(filepath.Clean(dir))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	return &Store{Dir: abs}, nil
}

func (s *Store) checkpointPath(key string) (string, error) {
	if !ValidKey(key) {
		return "", fmt.Errorf("unsafe checkpoint key %q", key)
	}
	return filepath.Join(s.Dir, key+".json"), nil
}

func (s *Store) bindingPath(jobID string) (string, error) {
	if !ValidKey(jobID) {
		return "", fmt.Errorf("unsafe job id %q", jobID)
	}
	return filepath.Join(s.Dir, jobID+".bind.json"), nil
}

// SaveCommitted atomically writes the task checkpoint and then its job
// binding. The checkpoint itself is already resumable by task key if the
// process exits between the two renames.
func (s *Store) SaveCommitted(t Task, result any) (*Checkpoint, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	cp := Checkpoint{
		SchemaVersion: SchemaVersion,
		Task:          t,
		Status:        StatusCommitted,
		Result:        raw,
	}
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	path, err := s.checkpointPath(t.Key)
	if err != nil {
		return nil, err
	}
	if err := WriteFileAtomic(path, data, 0o644); err != nil {
		return nil, err
	}
	if err := s.Bind(t); err != nil {
		return &cp, err
	}
	return &cp, nil
}

// Bind atomically records the current task identity for a logical job.
func (s *Store) Bind(t Task) error {
	path, err := s.bindingPath(t.JobID)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return WriteFileAtomic(path, data, 0o644)
}

// LoadKey reads a checkpoint by its bare task key without fingerprint checks.
func (s *Store) LoadKey(key string) (*Checkpoint, bool, error) {
	path, err := s.checkpointPath(key)
	if err != nil {
		return nil, false, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, false, fmt.Errorf("parse checkpoint %s: %w", path, err)
	}
	return &cp, true, nil
}

// LoadCommitted reads and verifies an exact task-key checkpoint.
func (s *Store) LoadCommitted(want Task) (*Checkpoint, bool, error) {
	cp, ok, err := s.LoadKey(want.Key)
	if err != nil || !ok {
		return cp, ok, err
	}
	if cp.Status != StatusCommitted {
		return nil, false, nil
	}
	if err := Verify(cp, want); err != nil {
		return cp, true, err
	}
	return cp, true, nil
}

// Resolve checks the logical job binding first. A binding to a different task
// is a fingerprint mismatch and is never silently discarded.
func (s *Store) Resolve(want Task) (*Checkpoint, bool, error) {
	bindPath, err := s.bindingPath(want.JobID)
	if err != nil {
		return nil, false, err
	}
	data, err := os.ReadFile(bindPath)
	if errors.Is(err, os.ErrNotExist) {
		return s.LoadCommitted(want)
	}
	if err != nil {
		return nil, false, err
	}
	var bound Task
	if err := json.Unmarshal(data, &bound); err != nil {
		return nil, false, fmt.Errorf("parse job binding %s: %w", bindPath, err)
	}
	if bound.Key != "" && bound.Key != want.Key {
		if old, ok, loadErr := s.LoadKey(bound.Key); loadErr == nil && ok {
			return old, true, Verify(old, want)
		}
		return nil, false, &MismatchError{
			Reasons: []string{fmt.Sprintf("job binding points to unavailable old task %s", bound.Key)},
			OldKey:  bound.Key,
			NewKey:  want.Key,
		}
	}
	return s.LoadCommitted(want)
}
