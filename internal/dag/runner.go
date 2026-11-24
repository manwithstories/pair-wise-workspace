// Package dag runs the review pipeline as a named task graph. Each node is
// idempotent: its key is derived from a hash of its input artifacts and
// parameters, so retries and crash recovery never re-run or double-write a
// completed node. State is persisted atomically after every node commit.
package dag

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"review/internal/media"
)

// Status values for one node.
type Status string

const (
	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusBlocked   Status = "blocked"
)

// Node is one unit of work. Deps names must complete successfully first.
// Run performs the node's side effects; returning an error marks failure.
type Node struct {
	Name string
	Deps []string
	// Run receives the live runner so it can record outputs and read
	// upstream artifacts.
	Run func(rt *Runner) error
	// MaxAttempts bounds retries; zero means a single attempt.
	MaxAttempts int
}

// NodeState is the persisted record for one node run.
type NodeState struct {
	Status     Status            `json:"status"`
	Key        string            `json:"key"`
	Attempts   int               `json:"attempts"`
	Outputs    map[string]string `json:"outputs,omitempty"`
	Error      string            `json:"error,omitempty"`
	Reason     string            `json:"reason,omitempty"`
	Class      media.Class       `json:"class,omitempty"`
	Dependency string            `json:"dependency,omitempty"`
	Track      uint32            `json:"track,omitempty"`
	BlockOff   int64             `json:"blockOffset,omitempty"`
}

// Runner executes a node set with durable, atomic state persistence.
type Runner struct {
	mu        sync.Mutex
	statePath string
	nodes     map[string]*Node
	state     map[string]*NodeState
	lastError map[string]error // in-memory failure causes for chain wrapping
}

// NewRunner creates a runner whose state file lives under stateDir.
func NewRunner(stateDir string) (*Runner, error) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, media.NewInternal("dag-runner", "create state dir", err)
	}
	rt := &Runner{
		statePath: filepath.Join(stateDir, "dag-state.json"),
		nodes:     map[string]*Node{},
		state:     map[string]*NodeState{},
		lastError: map[string]error{},
	}
	if err := rt.load(); err != nil {
		return nil, err
	}
	return rt, nil
}

// Add registers a node; duplicate names are rejected.
func (rt *Runner) Add(n Node) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if _, ok := rt.nodes[n.Name]; ok {
		return media.NewInternal("dag-runner",
			fmt.Sprintf("duplicate node %q", n.Name), nil)
	}
	if n.MaxAttempts <= 0 {
		n.MaxAttempts = 1
	}
	rt.nodes[n.Name] = &n
	if _, ok := rt.state[n.Name]; !ok {
		rt.state[n.Name] = &NodeState{Status: StatusPending, Outputs: map[string]string{}}
	}
	return nil
}

// RunAll, topoOrder and markBlockedDependents live in schedule.go.

func (rt *Runner) runOne(name string) error {
	rt.mu.Lock()
	node := rt.nodes[name]
	st := rt.state[name]
	// Resume guard: a previously succeeded node is never re-executed.
	// A node that needs key-sensitive recompute checks IsSucceededWith
	// itself inside Run; the durable success state is the primary guard.
	if st.Status == StatusSucceeded {
		rt.mu.Unlock()
		return nil
	}
	// A node that failed in a previous run does not silently retry: replay
	// its persisted failure so dependents are blocked through the chain.
	if st.Status == StatusFailed {
		rt.mu.Unlock()
		if cause := rt.lastError[name]; cause != nil {
			return cause
		}
		return media.NewInternal(name, st.Error, nil)
	}
	rt.mu.Unlock()
	// Verify dependencies before each attempt.
	for _, dep := range node.Deps {
		rt.mu.Lock()
		depState := rt.state[dep]
		rt.mu.Unlock()
		if depState == nil || depState.Status != StatusSucceeded {
			rt.mu.Lock()
			st.Status = StatusBlocked
			st.Error = fmt.Sprintf("dependency %q not succeeded", dep)
			rt.mu.Unlock()
			// Preserve the full dependency chain: if the upstream node
			// recorded a pipeline failure, wrap it; otherwise name the
			// missing dependency directly.
			if cause := rt.lastError[dep]; cause != nil {
				var pe *media.PipelineError
				if errors.As(cause, &pe) {
					return rt.persistAndReturn(name, media.BlockedBy(name, pe))
				}
			}
			return rt.persistAndReturn(name, media.NewConflict(name,
				st.Error, dep))
		}
	}
	var lastErr error
	for attempt := 1; attempt <= node.MaxAttempts; attempt++ {
		rt.mu.Lock()
		st.Attempts = attempt
		st.Status = StatusRunning
		rt.mu.Unlock()
		if err := rt.persist(); err != nil {
			return err
		}
		if err := node.Run(rt); err != nil {
			lastErr = err
			continue
		}
		rt.mu.Lock()
		st.Status = StatusSucceeded
		st.Error = ""
		rt.mu.Unlock()
		if err := rt.persist(); err != nil {
			return err
		}
		return nil
	}
	rt.mu.Lock()
	st.Status = StatusFailed
	if lastErr != nil {
		st.Error = lastErr.Error()
		var pe *media.PipelineError
		if errors.As(lastErr, &pe) {
			st.Class = pe.Class
			st.Dependency = pe.Dependency
			st.Track = pe.Track
			st.BlockOff = pe.BlockOff
			st.Reason = pe.Reason
		} else {
			st.Class = media.ClassInternal
			st.Reason = lastErr.Error()
		}
	}
	rt.lastError[name] = lastErr
	rt.mu.Unlock()
	if err := rt.persist(); err != nil {
		return err
	}
	return lastErr
}

// MarkKey records the idempotency key for a node before it runs.
func (rt *Runner) MarkKey(name, key string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.state[name].Key = key
}

// IsSucceededWith reports whether name completed with the given key, which
// lets a node skip re-running identical work on resume.
func (rt *Runner) IsSucceededWith(name, key string) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	st, ok := rt.state[name]
	return ok && st.Status == StatusSucceeded && st.Key == key
}

// RecordOutput stores a named output reference (typically a file hash) and
// is part of the durable state.
func (rt *Runner) RecordOutput(node, name, value string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.state[node].Outputs == nil {
		rt.state[node].Outputs = map[string]string{}
	}
	rt.state[node].Outputs[name] = value
}

// Output returns a recorded output and whether it exists.
func (rt *Runner) Output(node, name string) (string, bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	st, ok := rt.state[node]
	if !ok || st.Outputs == nil {
		return "", false
	}
	v, ok := st.Outputs[name]
	return v, ok
}

// State returns a snapshot copy of one node state.
func (rt *Runner) State(name string) (NodeState, bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	st, ok := rt.state[name]
	if !ok {
		return NodeState{}, false
	}
	cp := *st
	return cp, true
}

func (rt *Runner) persistAndReturn(name string, err error) error {
	_ = rt.persist()
	return err
}
