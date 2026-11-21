// Package dag schedules the pipeline as a dependency graph.  Each node has an
// idempotency key derived from its input hashes and configuration hash, so a
// rerun with identical inputs produces identical output hashes and is skipped
// entirely.  State is persisted to a local file after every transition; if a
// run is interrupted, only nodes that did not reach "done" are executed.
package dag

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mediapack/internal/fault"
)

// Status is a node's lifecycle state.
type Status string

const (
	StatusPending Status = "pending"
	StatusRunning Status = "running"
	StatusDone    Status = "done"
	StatusBlocked Status = "blocked"
	StatusFailed  Status = "failed"
)

// Limits bounds graph size.
type Limits struct{ MaxNodes int }

// DefaultLimits is deliberately small.
func DefaultLimits() Limits { return Limits{MaxNodes: 128} }

// Node is one unit of work.
type Node struct {
	Name   string
	Deps   []string
	Inputs []string // input hashes, order significant
	Config string   // configuration hash or canonical config text
	// Run performs the work and returns the output hash (or any stable
	// digest string) plus an error.  Run is called at most once per attempt.
	Run func() (string, error)
}

// State is the durable record of one run.
type State struct {
	Path  string            `json:"-"`
	Nodes map[string]Record `json:"nodes"`
}

// Record is one node's persisted state.
type Record struct {
	Key     string `json:"key"`
	Status  Status `json:"status"`
	Output  string `json:"output,omitempty"`
	Error   string `json:"error,omitempty"`
	Blocked string `json:"blocked_by,omitempty"`
}

// Key derives the idempotency key from ordered input hashes and config.
func (n Node) Key() string {
	h := sha256.New()
	writeField(h, n.Name)
	for _, in := range n.Inputs {
		writeField(h, in)
	}
	writeField(h, n.Config)
	return hex.EncodeToString(h.Sum(nil))
}

// Result summarizes a run.
type Result struct {
	Executed   []string
	Skipped    []string
	Resumed    []string
	Blocked    []string
	Hashes     map[string]string
	FirstBlock *fault.Error
}

// Load reads state from path, or returns an empty state if the file does not
// exist.  A corrupt state file is an input error: silently starting over
// would make a resumed run impossible to distinguish from a fresh one.
func Load(path string) (*State, error) {
	st := &State{Path: path, Nodes: make(map[string]Record)}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return st, nil
	}
	if err != nil {
		return nil, fault.Wrap(fault.ClassInput, "dag", "*", "state-read-failed",
			"cannot read state file", err)
	}
	if err := json.Unmarshal(data, st); err != nil {
		return nil, fault.Wrap(fault.ClassInput, "dag", "*", "state-corrupt",
			"state file is not valid JSON", err)
	}
	if st.Nodes == nil {
		st.Nodes = make(map[string]Record)
	}
	st.Path = path
	return st, nil
}

// Save writes state atomically: a crash mid-write must leave either the old
// or the new file, never a truncated one.
func (s *State) Save() error {
	if s.Path == "" {
		return fault.New(fault.ClassInternal, "dag", "*", "no-state-path", "state has no path")
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return fault.Wrap(fault.ClassInternal, "dag", "*", "state-dir-failed",
			"cannot create state directory", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fault.Wrap(fault.ClassInternal, "dag", "*", "state-encode-failed",
			"cannot encode state", err)
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fault.Wrap(fault.ClassInternal, "dag", "*", "state-write-failed",
			"cannot write state", err)
	}
	if err := os.Rename(tmp, s.Path); err != nil {
		return fault.Wrap(fault.ClassInternal, "dag", "*", "state-rename-failed",
			"cannot publish state", err)
	}
	return nil
}

// Run executes the graph in dependency order.  Nodes whose key matches a
// done record are skipped; with resume set, pending or failed nodes from the
// same key are executed, and previously done nodes are reported as resumed.
// Without resume, a done record is still honored so that the output hash is
// reproducible; set resume=false only when callers want a fresh execution.
func Run(nodes []Node, state *State, resume bool) (*Result, error) {
	if len(nodes) > DefaultLimits().MaxNodes {
		return nil, fault.New(fault.ClassLimit, "dag", "*", "too-many-nodes",
			fmt.Sprintf("%d nodes exceeds limit %d", len(nodes), DefaultLimits().MaxNodes))
	}
	byName := make(map[string]Node, len(nodes))
	for _, n := range nodes {
		if n.Name == "" {
			return nil, fault.New(fault.ClassInput, "dag", "*", "empty-node-name", "node has no name")
		}
		if _, dup := byName[n.Name]; dup {
			return nil, fault.New(fault.ClassInput, "dag", n.Name, "duplicate-node",
				"node name appears more than once")
		}
		if n.Run == nil {
			return nil, fault.New(fault.ClassInternal, "dag", n.Name, "nil-run", "node has no run function")
		}
		byName[n.Name] = n
	}
	order, err := topoOrder(nodes, byName)
	if err != nil {
		return nil, err
	}

	res := &Result{Hashes: make(map[string]string, len(nodes))}
	for _, name := range order {
		n := byName[name]
		key := n.Key()
		rec, seen := state.Nodes[name]

		// A done node with the same key is fully reproducible: reuse it.
		if seen && rec.Status == StatusDone && rec.Key == key {
			res.Skipped = append(res.Skipped, name)
			if resume {
				res.Resumed = append(res.Resumed, name)
			}
			res.Hashes[name] = rec.Output
			continue
		}

		// Any dependency not done blocks this node; record the first such
		// dependency so the terminal can name it exactly.
		if dep, blocked := firstBlockedDep(n, state, res.Hashes); blocked {
			rec = Record{Key: key, Status: StatusBlocked, Blocked: dep}
			state.Nodes[name] = rec
			_ = state.Save()
			res.Blocked = append(res.Blocked, name)
			if res.FirstBlock == nil {
				res.FirstBlock = fault.Blocked("dag", name, "dag:"+dep, 0, 0)
			}
			continue
		}

		state.Nodes[name] = Record{Key: key, Status: StatusRunning}
		if err := state.Save(); err != nil {
			return nil, err
		}
		out, err := n.Run()
		if err != nil {
			fe, ok := fault.As(err)
			if !ok {
				fe = fault.Wrap(fault.ClassInternal, "dag", name, "node-failed", "node failed", err)
			}
			state.Nodes[name] = Record{Key: key, Status: StatusFailed, Error: fe.Error()}
			_ = state.Save()
			// A failed node is itself the earliest blocking dependency; keep
			// its original attribution (frame, track) and only fill in the
			// naming field when the stage did not supply one.
			if fe.BlockedBy == "" {
				fe.BlockedBy = "dag:" + name
			}
			if res.FirstBlock == nil {
				res.FirstBlock = fe
			}
			res.Blocked = append(res.Blocked, name)
			continue
		}
		state.Nodes[name] = Record{Key: key, Status: StatusDone, Output: out}
		if err := state.Save(); err != nil {
			return nil, err
		}
		res.Executed = append(res.Executed, name)
		res.Hashes[name] = out
	}
	return res, nil
}

// firstBlockedDep returns the first dependency that is not done for this key.
func firstBlockedDep(n Node, state *State, hashes map[string]string) (string, bool) {
	for _, dep := range n.Deps {
		rec, ok := state.Nodes[dep]
		if !ok || rec.Status != StatusDone {
			return dep, true
		}
		if _, ok := hashes[dep]; !ok {
			hashes[dep] = rec.Output
		}
	}
	return "", false
}

// topoOrder returns a deterministic topological order, ties broken by name.
func topoOrder(nodes []Node, byName map[string]Node) ([]string, error) {
	indegree := make(map[string]int, len(nodes))
	dependents := make(map[string][]string, len(nodes))
	for _, n := range nodes {
		indegree[n.Name] = len(n.Deps)
		for _, dep := range n.Deps {
			if _, ok := byName[dep]; !ok {
				return nil, fault.New(fault.ClassInput, "dag", n.Name, "missing-dependency",
					fmt.Sprintf("node depends on unknown %q", dep))
			}
			dependents[dep] = append(dependents[dep], n.Name)
		}
	}
	var ready []string
	for name, deg := range indegree {
		if deg == 0 {
			ready = append(ready, name)
		}
	}
	sort.Strings(ready)
	var order []string
	for len(ready) > 0 {
		name := ready[0]
		ready = ready[1:]
		order = append(order, name)
		for _, next := range dependents[name] {
			indegree[next]--
			if indegree[next] == 0 {
				ready = append(ready, next)
			}
		}
		sort.Strings(ready)
	}
	if len(order) != len(nodes) {
		var cyclic []string
		for name, deg := range indegree {
			if deg > 0 {
				cyclic = append(cyclic, name)
			}
		}
		sort.Strings(cyclic)
		return nil, fault.New(fault.ClassInput, "dag", "*", "cycle",
			"graph contains a cycle involving "+strings.Join(cyclic, ","))
	}
	return order, nil
}

func writeField(h interface{ Write([]byte) (int, error) }, s string) {
	var size [8]byte
	n := uint64(len(s))
	for i := 7; i >= 0; i-- {
		size[i] = byte(n)
		n >>= 8
	}
	h.Write(size[:])
	h.Write([]byte(s))
}
