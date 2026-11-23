// Package scheduler executes the archive pipeline as a DAG of versioned
// nodes with idempotent retries: identical inputs, parameters and node
// versions always produce identical outputs, and completed nodes never
// produce output twice.
package scheduler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"archivecheck/internal/fault"
)

// Node is one pipeline step. Run must be deterministic: its output may only
// depend on its parameters and its dependencies' outputs.
type Node struct {
	Name    string
	Version int
	Deps    []string
	Run     func(c *Ctx) (any, error)
}

// Ctx gives a node access to its dependencies' outputs.
type Ctx struct {
	runner *Runner
}

// Dep unmarshals the recorded output of a dependency into out.
func (c *Ctx) Dep(name string, out any) error {
	rec, ok := c.runner.state.Nodes[name]
	if !ok {
		return fmt.Errorf("scheduler: dependency %q has no recorded output", name)
	}
	return json.Unmarshal(rec.Output, out)
}

// NodeRecord is the persisted result of one completed node.
type NodeRecord struct {
	Name       string          `json:"name"`
	Version    int             `json:"version"`
	InputHash  string          `json:"input_hash"`
	OutputHash string          `json:"output_hash"`
	Output     json.RawMessage `json:"output"`
}

// State is the resumable pipeline state.
type State struct {
	ParamsHash string                 `json:"params_hash"`
	Nodes      map[string]*NodeRecord `json:"nodes"`
}

// Report summarizes one Run call.
type Report struct {
	RerunNodes       int          `json:"rerun_nodes"`
	SkippedNodes     int          `json:"skipped_nodes"`
	Completed        bool         `json:"completed"`
	Interrupted      bool         `json:"interrupted"`
	InterruptedAfter string       `json:"interrupted_after,omitempty"`
	BlockedBy        string       `json:"blocked_by,omitempty"`
	Fault            *fault.Fault `json:"fault,omitempty"`
}

// Runner executes nodes in dependency order against a persistent state.
type Runner struct {
	paramsHash string
	nodes      []*Node
	byName     map[string]*Node
	state      *State
}

// New creates a runner. paramsHash must change whenever any run parameter
// that can affect node output changes.
func New(paramsHash string) *Runner {
	return &Runner{
		paramsHash: paramsHash,
		byName:     map[string]*Node{},
		state:      &State{ParamsHash: paramsHash, Nodes: map[string]*NodeRecord{}},
	}
}

// Add registers a node. Nodes must be added in topological order.
func (r *Runner) Add(n *Node) error {
	if n.Name == "" || n.Run == nil {
		return fmt.Errorf("scheduler: node needs a name and a Run func")
	}
	if _, dup := r.byName[n.Name]; dup {
		return fmt.Errorf("scheduler: duplicate node %q", n.Name)
	}
	for _, d := range n.Deps {
		if _, ok := r.byName[d]; !ok {
			return fmt.Errorf("scheduler: node %q depends on unknown node %q", n.Name, d)
		}
	}
	r.nodes = append(r.nodes, n)
	r.byName[n.Name] = n
	return nil
}

func hashParts(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (r *Runner) inputHash(n *Node) string {
	parts := []string{r.paramsHash, n.Name, fmt.Sprint(n.Version)}
	for _, d := range n.Deps {
		parts = append(parts, d, r.state.Nodes[d].OutputHash)
	}
	return hashParts(parts...)
}

// ErrInterrupted is returned (inside the report, not as error) — Run stops
// cleanly after the named node and persists state.
func (r *Runner) Run(stopAfter string) (*Report, error) {
	rep := &Report{}
	for _, n := range r.nodes {
		ih := r.inputHash(n)
		if rec, ok := r.state.Nodes[n.Name]; ok && rec.InputHash == ih {
			rep.SkippedNodes++ // already produced: do not run again
			continue
		}
		out, err := n.Run(&Ctx{runner: r})
		if err != nil {
			var fl *fault.Fault
			if errors.As(err, &fl) {
				rep.BlockedBy = n.Name
				rep.Fault = fl
				return rep, nil
			}
			return rep, fmt.Errorf("scheduler: node %s: %w", n.Name, err)
		}
		raw, err := json.Marshal(out)
		if err != nil {
			return rep, fmt.Errorf("scheduler: node %s output: %w", n.Name, err)
		}
		oh := hashParts(string(raw))
		r.state.Nodes[n.Name] = &NodeRecord{
			Name: n.Name, Version: n.Version, InputHash: ih,
			OutputHash: oh, Output: raw,
		}
		rep.RerunNodes++
		if n.Name == stopAfter {
			rep.Interrupted = true
			rep.InterruptedAfter = n.Name
			return rep, nil
		}
	}
	rep.Completed = true
	return rep, nil
}

// Output unmarshals a completed node's output (for the final report).
func (r *Runner) Output(name string, out any) error {
	rec, ok := r.state.Nodes[name]
	if !ok {
		return fmt.Errorf("scheduler: node %q not completed", name)
	}
	return json.Unmarshal(rec.Output, out)
}

// OutputHash returns the recorded output hash of a node.
func (r *Runner) OutputHash(name string) string {
	if rec, ok := r.state.Nodes[name]; ok {
		return rec.OutputHash
	}
	return ""
}
