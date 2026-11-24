package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"bcast/media"
)

// Checkpoint persists node states and idempotency results to a local JSON
// file. Writes go through a temp file plus fsync and an atomic rename, so a
// crash leaves either the previous checkpoint or the new one.
type Checkpoint struct {
	Version int                  `json:"version"`
	Nodes   map[string]NodeState `json:"nodes"`
	Results map[string]string    `json:"results"`        // idempotency key -> result
	Meta    map[string]string    `json:"meta,omitempty"` // run parameters and summary
	path    string
	mu      sync.Mutex
}

// CheckpointVersion is the on-disk schema version. Older files without a
// version are read as version 1.
const CheckpointVersion = 2

// NewCheckpoint creates an empty checkpoint; path "" disables persistence.
func NewCheckpoint(path string) *Checkpoint {
	return &Checkpoint{
		Version: CheckpointVersion,
		path:    path,
		Nodes:   map[string]NodeState{},
		Results: map[string]string{},
		Meta:    map[string]string{},
	}
}

// SetMeta stores a run-level key/value, persisted with the rest of the
// checkpoint. It is how the demo hands the verify command its parameters and
// metrics without a second file.
func (c *Checkpoint) SetMeta(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Meta == nil {
		c.Meta = map[string]string{}
	}
	c.Meta[key] = value
}

// GetMeta reads a run-level value.
func (c *Checkpoint) GetMeta(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.Meta[key]
	return v, ok
}

// ErrCheckpointCorrupt reports an unreadable checkpoint. Callers that can
// restart from scratch may treat it as empty after moving it aside.
var ErrCheckpointCorrupt = errors.New("checkpoint corrupt")

// LoadCheckpoint reads path, returning an empty checkpoint when it does not
// exist yet. A malformed file yields ErrCheckpointCorrupt so the caller can
// decide to discard it rather than silently reusing garbage.
func LoadCheckpoint(path string) (*Checkpoint, error) {
	cp := NewCheckpoint(path)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cp, nil
		}
		return nil, fmt.Errorf("checkpoint read: %w", err)
	}
	if len(data) == 0 {
		return cp, nil
	}
	if err := json.Unmarshal(data, cp); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCheckpointCorrupt, err)
	}
	cp.path = path
	if cp.Version == 0 {
		cp.Version = 1
	}
	if cp.Nodes == nil {
		cp.Nodes = map[string]NodeState{}
	}
	if cp.Results == nil {
		cp.Results = map[string]string{}
	}
	if cp.Meta == nil {
		cp.Meta = map[string]string{}
	}
	return cp, nil
}

// Save writes the checkpoint atomically (temp file, fsync, rename).
func (c *Checkpoint) Save() error {
	if c.path == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Version = CheckpointVersion
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(c.path)
	tmp, err := os.CreateTemp(dir, ".bcast-cp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, c.path)
}

// HasResult reports whether an idempotency key already has a result.
func (c *Checkpoint) HasResult(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.Results[key]
	return v, ok
}

// putResult records a result under key, first-write-wins: a repeated write
// for the same key never changes the stored value, so re-execution cannot
// alter a result already committed.
func (c *Checkpoint) putResult(key, result string) {
	if key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.Results[key]; exists {
		return
	}
	c.Results[key] = result
}

// RunIdempotent executes fn unless the checkpoint already holds a result for
// key. Recoverable errors complete the node with their partial result and
// mark it recovered. Other errors are retried up to maxRetries; a later
// success also marks recovery. The returned attempts counts real executions
// of fn; 0 means the result was reused and fn was not called.
func RunIdempotent(ctx context.Context, cp *Checkpoint, key string, maxRetries int, fn func(ctx context.Context) (string, error)) (result string, attempts int, recovered bool, err error) {
	if key != "" {
		if v, ok := cp.HasResult(key); ok {
			return v, 0, false, nil // already produced: do not re-execute or rewrite
		}
	}
	for attempt := 0; ; attempt++ {
		if cerr := ctx.Err(); cerr != nil {
			return "", attempt, false, cerr
		}
		attempts++
		result, err = fn(ctx)
		if err == nil {
			cp.putResult(key, result)
			return result, attempts, attempt > 0, nil
		}
		if media.ClassOf(err) == media.ClassRecoverable {
			cp.putResult(key, result)
			return result, attempts, true, nil
		}
		if attempt >= maxRetries {
			return "", attempts, false, err
		}
	}
}
