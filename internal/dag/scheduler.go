// Package dag runs dependency-ordered nodes idempotently. A node's key is
// derived from its input digests and parameter digest, so repeated
// submissions with identical inputs converge on one persisted result.
package dag

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"medpipe/internal/model"
)

// State is the lifecycle of a node execution.
type State string

const (
	StatePending State = "pending"
	StateReady   State = "ready"
	StateDone    State = "done"
	StateFailed  State = "failed"
	StateBlocked State = "blocked"
	StateReused  State = "reused"
)

// Node is one task. Deps are node names that must finish first.
type Node struct {
	Name   string
	Deps   []string
	Params any
	// Run executes the node. It must be pure with respect to Key: running
	// the same key twice must produce byte-identical output.
	Run func(key string) (Summary, error)
}

// Summary is what a completed node persists. Digest covers the output bytes
// and feeds downstream keys.
type Summary struct {
	Digest [32]byte
	Detail string
}

// InputDigests lists upstream node digests that key this node, in the order
// the node declared its dependencies (sorted for stability).
type InputDigests []string

// Result is the recorded outcome of one node.
type Result struct {
	Name   string
	Key    string
	State  State
	Digest [32]byte
	Detail string
	Error  string
	// Reused reports that an existing result satisfied this submission.
	Reused bool
	// Runs is how many times Run actually executed for this key.
	Runs int
}

// Options tunes scheduler behavior.
type Options struct {
	// MaxAttempts bounds retries per node. Zero means 1 (no retry).
	MaxAttempts int
	// RetryFailedDeps retries failed dependencies before blocking successors.
	RetryFailedDeps bool
}

// Scheduler owns the node graph and the persistent result cache.
type Scheduler struct {
	mu      sync.Mutex
	nodes   map[string]*Node
	order   []string
	cache   *Store
	opt     Options
	results map[string]Result
	runs    map[string]int
	// inflight deduplicates concurrent submissions of the same node name.
	inflight map[string]*sync.WaitGroup
}

// New builds a scheduler. nodes are validated for unknown and cyclic deps.
func New(nodes []*Node, store *Store, opt Options) (*Scheduler, error) {
	if store == nil {
		return nil, model.New(model.KindInput, "dag.New", "nil result store")
	}
	if opt.MaxAttempts <= 0 {
		opt.MaxAttempts = 1
	}
	s := &Scheduler{
		nodes:    make(map[string]*Node, len(nodes)),
		cache:    store,
		opt:      opt,
		results:  make(map[string]Result),
		runs:     make(map[string]int),
		inflight: make(map[string]*sync.WaitGroup),
	}
	for _, n := range nodes {
		if n == nil || n.Name == "" {
			return nil, model.New(model.KindInput, "dag.New", "node with empty name")
		}
		if n.Run == nil {
			return nil, model.New(model.KindInput, "dag.New", fmt.Sprintf("node %s has no run function", n.Name))
		}
		if _, dup := s.nodes[n.Name]; dup {
			return nil, model.New(model.KindInput, "dag.New", fmt.Sprintf("duplicate node %q", n.Name))
		}
		s.nodes[n.Name] = n
		s.order = append(s.order, n.Name)
	}
	for _, n := range nodes {
		for _, d := range n.Deps {
			if _, ok := s.nodes[d]; !ok {
				return nil, model.New(model.KindInput, "dag.New", fmt.Sprintf("node %s depends on unknown node %q", n.Name, d))
			}
		}
	}
	order, err := topoSort(s.nodes)
	if err != nil {
		return nil, err
	}
	s.order = order
	return s, nil
}

// Order returns the validated topological order.
func (s *Scheduler) Order() []string {
	out := make([]string, len(s.order))
	copy(out, s.order)
	return out
}

func topoSort(nodes map[string]*Node) ([]string, error) {
	names := make([]string, 0, len(nodes))
	for n := range nodes {
		names = append(names, n)
	}
	sort.Strings(names)
	indeg := make(map[string]int, len(names))
	dependents := make(map[string][]string, len(names))
	for _, n := range names {
		node := nodes[n]
		seen := map[string]bool{}
		for _, d := range node.Deps {
			if seen[d] {
				return nil, model.New(model.KindInput, "dag.topoSort", fmt.Sprintf("node %s lists dependency %s twice", n, d))
			}
			seen[d] = true
			indeg[n]++
			dependents[d] = append(dependents[d], n)
		}
	}
	var ready []string
	for _, n := range names {
		if indeg[n] == 0 {
			ready = append(ready, n)
		}
	}
	var out []string
	for len(ready) > 0 {
		sort.Strings(ready)
		n := ready[0]
		ready = ready[1:]
		out = append(out, n)
		for _, d := range dependents[n] {
			indeg[d]--
			if indeg[d] == 0 {
				ready = append(ready, d)
			}
		}
	}
	if len(out) != len(names) {
		var cyc []string
		for _, n := range names {
			if indeg[n] > 0 {
				cyc = append(cyc, n)
			}
		}
		sort.Strings(cyc)
		return nil, model.New(model.KindInput, "dag.topoSort", fmt.Sprintf("dependency cycle among %v", cyc))
	}
	return out, nil
}

// Key computes the idempotency key for a node given upstream digests.
func Key(name string, inputDigests InputDigests, params any) (string, error) {
	pb, err := canonicalJSON(params)
	if err != nil {
		return "", model.Wrap(model.KindInput, "dag.Key", err)
	}
	h := sha256.New()
	h.Write([]byte("medpipe/node/v1\x00"))
	h.Write([]byte(name))
	h.Write([]byte{0})
	ids := append(InputDigests(nil), inputDigests...)
	sort.Strings(ids)
	for _, d := range ids {
		h.Write([]byte(d))
		h.Write([]byte{0})
	}
	h.Write(pb)
	return hex.EncodeToString(h.Sum(nil)), nil
}

func canonicalJSON(v any) ([]byte, error) {
	if v == nil {
		return []byte("null"), nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	// Marshal is map-key-sorted in encoding/json, which is what canonical
	// means here; the helper exists to centralize the nil case.
	return b, nil
}
