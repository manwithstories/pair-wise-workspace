package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"bcast/media"
)

// Checkpoint persists node states and idempotency results to a local JSON
// file. Writes go through a temp file plus atomic rename.
type Checkpoint struct {
	path    string
	Nodes   map[string]NodeState `json:"nodes"`
	Results map[string]string    `json:"results"` // idempotency key -> result
	mu      sync.Mutex
}

// NewCheckpoint creates an empty checkpoint; path "" disables persistence.
func NewCheckpoint(path string) *Checkpoint {
	return &Checkpoint{
		path:    path,
		Nodes:   map[string]NodeState{},
		Results: map[string]string{},
	}
}

// LoadCheckpoint reads path, returning an empty checkpoint when it does
// not exist yet.
func LoadCheckpoint(path string) (*Checkpoint, error) {
	cp := NewCheckpoint(path)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cp, nil
		}
		return nil, fmt.Errorf("checkpoint read: %w", err)
	}
	if err := json.Unmarshal(data, cp); err != nil {
		return nil, fmt.Errorf("checkpoint parse: %w", err)
	}
	if cp.Nodes == nil {
		cp.Nodes = map[string]NodeState{}
	}
	if cp.Results == nil {
		cp.Results = map[string]string{}
	}
	return cp, nil
}

// Save writes the checkpoint atomically (temp file + rename).
func (c *Checkpoint) Save() error {
	if c.path == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(c.path)
	tmp, err := os.CreateTemp(dir, ".bcast-cp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), c.path)
}

// HasResult reports whether an idempotency key already has a result.
func (c *Checkpoint) HasResult(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.Results[key]
	return v, ok
}

// RunIdempotent executes fn unless the checkpoint already holds a result
// for key. Recoverable errors complete the node with their partial result
// and mark it recovered. Other errors are retried up to maxRetries; a
// later success also marks recovery. The returned attempts counts real
// executions of fn.
func RunIdempotent(ctx context.Context, cp *Checkpoint, key string, maxRetries int, fn func(ctx context.Context) (string, error)) (result string, attempts int, recovered bool, err error) {
	if key != "" {
		if v, ok := cp.HasResult(key); ok {
			return v, 0, false, nil // already produced: do not re-execute or rewrite
		}
	}
	for attempt := 0; ; attempt++ {
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

func (c *Checkpoint) putResult(key, result string) {
	if key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Results[key] = result
}
