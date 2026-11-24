// Package schedule executes the analysis pipeline as a dependency DAG with
// bounded concurrency, idempotent retries and deterministic first-blocker
// reporting.
package schedule

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
)

// Status is the exhaustive node state machine required by the spec.
type Status string

const (
	StatusPending Status = "pending" // 待执行
	StatusRunning Status = "running" // 运行中
	StatusSuccess Status = "success" // 成功
	StatusFailed  Status = "failed"  // 失败（重试已耗尽）
	StatusBlocked Status = "blocked" // 阻塞（上游失败，无法运行）
)

// Func is the unit of work a node runs. Implementations must be idempotent
// for a given key: re-running after an interruption must reproduce identical
// outputs and hashes.
type Func func(ctx *RunContext) error

// Node is one DAG vertex.
type Node struct {
	ID   string
	Deps []string
	// InputSummary and ParamSummary feed the idempotency key. They capture the
	// inputs the node consumes and the parameters that change its behavior.
	InputSummary string
	ParamSummary string
	Run          Func
}

// Key returns the deterministic idempotency key for a node execution.
func (n *Node) Key() string {
	h := sha256.New()
	h.Write([]byte(n.ID))
	h.Write([]byte{0})
	h.Write([]byte(n.InputSummary))
	h.Write([]byte{0})
	h.Write([]byte(n.ParamSummary))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// DAG is an immutable graph definition; validation happens at Build.
type DAG struct {
	nodes map[string]*Node
	order []string // topological order, deterministic by construction
}

// NewDAG validates nodes (unique ids, known deps, no cycles) and computes a
// stable topological ordering.
func NewDAG(nodes []Node) (*DAG, error) {
	d := &DAG{nodes: map[string]*Node{}}
	for i := range nodes {
		n := nodes[i]
		if n.ID == "" {
			return nil, errDAG("node has empty id")
		}
		if _, dup := d.nodes[n.ID]; dup {
			return nil, errDAG("duplicate node id: " + n.ID)
		}
		d.nodes[n.ID] = &n
	}
	for _, n := range d.nodes {
		for _, dep := range n.Deps {
			if _, ok := d.nodes[dep]; !ok {
				return nil, errDAG("node " + n.ID + " depends on unknown " + dep)
			}
		}
	}
	order, err := topoSort(d.nodes)
	if err != nil {
		return nil, err
	}
	d.order = order
	return d, nil
}

// Order returns the topological node ids.
func (d *DAG) Order() []string { return append([]string(nil), d.order...) }

func topoSort(nodes map[string]*Node) ([]string, error) {
	indeg := map[string]int{}
	adj := map[string][]string{}
	for id, n := range nodes {
		indeg[id] = len(n.Deps)
		for _, dep := range n.Deps {
			adj[dep] = append(adj[dep], id)
		}
	}
	// Seed with zero-indegree nodes sorted by id for deterministic ordering.
	var ready []string
	for id := range nodes {
		if indeg[id] == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	var order []string
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		children := append([]string(nil), adj[id]...)
		sort.Strings(children)
		for _, child := range children {
			indeg[child]--
			if indeg[child] == 0 {
				ready = insertSorted(ready, child)
			}
		}
	}
	if len(order) != len(nodes) {
		return nil, errDAG("dependency cycle detected")
	}
	return order, nil
}

func insertSorted(s []string, v string) []string {
	i := sort.SearchStrings(s, v)
	s = append(s, "")
	copy(s[i+1:], s[i:])
	s[i] = v
	return s
}
