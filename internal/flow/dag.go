// Package flow provides the deterministic dependency graph used to execute
// initialization, repair, indexing, and verification tasks.
package flow

import (
	"sort"

	"github.com/pairwise/mediad/internal/media"
	"github.com/pairwise/mediad/internal/metric"
)

// State is a node's persisted lifecycle state.
type State string

const (
	StatePending State = "pending"
	StateRunning State = "running"
	StateDone    State = "done"
	StateBlocked State = "blocked"
	StateFailed  State = "failed"
)

// ProduceFunc creates a node's artifact. Retries must be deterministic for a
// fixed InputHash so the resulting SHA-256 does not change.
type ProduceFunc func() ([]byte, error)

// Node is a DAG vertex.
type Node struct {
	ID        string            `json:"id"`
	Deps      []string          `json:"deps,omitempty"`
	Inputs    map[string]string `json:"inputs,omitempty"`
	OutputRel string            `json:"output_rel,omitempty"`
	Retries   int               `json:"-"`
	Produce   ProduceFunc       `json:"-"`

	InputHash string `json:"input_hash"`
	State     State  `json:"state"`
	Attempts  int    `json:"attempts"`
	SHA256    string `json:"sha256,omitempty"`
	Detail    string `json:"detail,omitempty"`
	BlockedBy string `json:"blocked_by,omitempty"`
}

// DAG is a validated set of nodes.
type DAG struct {
	nodes map[string]*Node
}

// New creates an empty DAG.
func New() *DAG { return &DAG{nodes: make(map[string]*Node)} }

// Add inserts a node and validates duplicate IDs and dependency existence.
func (d *DAG) Add(n *Node) error {
	if n == nil || n.ID == "" {
		return media.NewError(media.CodeInvalidArgument, "flow.dag", "id", "node id required")
	}
	if _, exists := d.nodes[n.ID]; exists {
		return media.NewError(media.CodeInvalidArgument, "flow.dag", "id", "duplicate node: "+n.ID)
	}
	// Dependencies may be added in any order; TopologicalOrder validates them.
	if n.State == "" {
		n.State = StatePending
	}
	n.InputHash = metric.InputsHash(n.Inputs)
	d.nodes[n.ID] = n
	return nil
}

// Get returns a node.
func (d *DAG) Get(id string) *Node { return d.nodes[id] }

// IDs returns sorted node IDs.
func (d *DAG) IDs() []string {
	ids := make([]string, 0, len(d.nodes))
	for id := range d.nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// TopologicalOrder returns a stable Kahn order (ties sorted by ID).
func (d *DAG) TopologicalOrder() ([]string, error) {
	indeg := make(map[string]int, len(d.nodes))
	dependents := make(map[string][]string, len(d.nodes))
	for id, n := range d.nodes {
		for _, dep := range n.Deps {
			if _, ok := d.nodes[dep]; !ok {
				return nil, media.NewError(media.CodeInvalidArgument, "flow.dag", "deps", id+" references missing dep "+dep)
			}
		}
		indeg[id] = len(n.Deps)
		for _, dep := range n.Deps {
			dependents[dep] = append(dependents[dep], id)
		}
	}
	ready := make([]string, 0)
	for id, deg := range indeg {
		if deg == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	var order []string
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		next := append([]string(nil), dependents[id]...)
		sort.Strings(next)
		for _, child := range next {
			indeg[child]--
			if indeg[child] == 0 {
				ready = append(ready, child)
			}
		}
		sort.Strings(ready)
	}
	if len(order) != len(d.nodes) {
		cycle := make([]string, 0)
		for id, deg := range indeg {
			if deg > 0 {
				cycle = append(cycle, id)
			}
		}
		sort.Strings(cycle)
		return nil, media.NewError(media.CodeInvalidArgument, "flow.dag", "deps", "dependency cycle: "+join(cycle))
	}
	return order, nil
}

func join(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += ","
		}
		out += x
	}
	return out
}

// FirstBlocked returns the first blocked node in stable topological order.
func (d *DAG) FirstBlocked() (string, error) {
	order, err := d.TopologicalOrder()
	if err != nil {
		return "", err
	}
	for _, id := range order {
		if d.nodes[id].State == StateBlocked {
			return id, nil
		}
	}
	return "", nil
}
