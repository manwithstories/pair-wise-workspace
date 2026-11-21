// Package dag schedules media-processing nodes in dependency order, runs
// them with bounded concurrency and records enough evidence for deterministic
// retries and checkpoint recovery.
package dag

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
)

// Node is one unit of work. Inputs are upstream node ids; InputDigest is the
// hash of the node's own source data and together with the id forms the
// idempotent key.
type Node struct {
	ID          string
	Inputs      []string
	InputDigest string
	// Run executes the node. It must be pure with respect to key inputs:
	// equal (ID, InputDigest) must produce equal outputs.
	Run func(ctx *RunContext) error
}

// Edge is an explicit dependency record, useful for reports and tests.
type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Graph is an immutable-ish node set validated before execution.
type Graph struct {
	nodes map[string]*Node
	order []string
}

// NewGraph validates unique ids, known inputs and acyclicity, and computes a
// deterministic topological order (ties break by node id).
func NewGraph(nodes []Node) (*Graph, error) {
	g := &Graph{nodes: map[string]*Node{}}
	for i := range nodes {
		n := nodes[i]
		if n.ID == "" {
			return nil, fmt.Errorf("dag: node #%d has empty id", i)
		}
		if _, dup := g.nodes[n.ID]; dup {
			return nil, fmt.Errorf("dag: duplicate node id %q", n.ID)
		}
		g.nodes[n.ID] = &n
	}
	indeg := make(map[string]int, len(nodes))
	children := make(map[string][]string)
	for _, n := range g.nodes {
		indeg[n.ID] = 0
	}
	for _, n := range g.nodes {
		seen := map[string]bool{}
		for _, in := range n.Inputs {
			if _, ok := g.nodes[in]; !ok {
				return nil, fmt.Errorf("dag: node %q depends on unknown node %q", n.ID, in)
			}
			if seen[in] {
				return nil, fmt.Errorf("dag: duplicate edge %s -> %s", in, n.ID)
			}
			seen[in] = true
			indeg[n.ID]++
			children[in] = append(children[in], n.ID)
		}
	}
	// Kahn's algorithm with sorted ids gives a stable topological order.
	ready := make([]string, 0)
	for id, d := range indeg {
		if d == 0 {
			ready = append(ready, id)
		}
	}
	order := make([]string, 0, len(nodes))
	for len(ready) > 0 {
		sort.Strings(ready)
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		next := append([]string{}, children[id]...)
		sort.Strings(next)
		for _, ch := range next {
			indeg[ch]--
			if indeg[ch] == 0 {
				ready = append(ready, ch)
			}
		}
	}
	if len(order) != len(nodes) {
		return nil, fmt.Errorf("dag: cycle detected, resolved %d of %d nodes", len(order), len(nodes))
	}
	g.order = order
	return g, nil
}

// Order returns the computed topological order.
func (g *Graph) Order() []string { return append([]string{}, g.order...) }

// Edges returns all dependency edges in deterministic order.
func (g *Graph) Edges() []Edge {
	var out []Edge
	for _, id := range g.order {
		ins := append([]string{}, g.nodes[id].Inputs...)
		sort.Strings(ins)
		for _, in := range ins {
			out = append(out, Edge{From: in, To: id})
		}
	}
	return out
}

// Key is the idempotent key of a node: its id plus every transitive input
// digest, so any upstream change invalidates the node.
type Key struct{ Value string }

// IdempotentKey hashes node id, its own digest and the digests of all
// transitive inputs (already topological), guaranteeing that changed input
// summaries produce a different key.
func (g *Graph) IdempotentKey(id string, upstreamKeys map[string]Key) (Key, error) {
	n, ok := g.nodes[id]
	if !ok {
		return Key{}, fmt.Errorf("dag: unknown node %q", id)
	}
	h := sha256.New()
	fmt.Fprintf(h, "node=%s\ndigest=%s\n", n.ID, n.InputDigest)
	ins := append([]string{}, n.Inputs...)
	sort.Strings(ins)
	for _, in := range ins {
		k, ok := upstreamKeys[in]
		if !ok {
			return Key{}, fmt.Errorf("dag: upstream key missing for %s -> %s", in, id)
		}
		fmt.Fprintf(h, "in=%s key=%s\n", in, k.Value)
	}
	return Key{Value: hex.EncodeToString(h.Sum(nil))}, nil
}

// AllKeys computes idempotent keys for every node in topological order.
func (g *Graph) AllKeys() (map[string]Key, error) {
	keys := make(map[string]Key, len(g.order))
	for _, id := range g.order {
		k, err := g.IdempotentKey(id, keys)
		if err != nil {
			return nil, err
		}
		keys[id] = k
	}
	return keys, nil
}

// FirstBlocker walks the topological order from the beginning and returns the
// first failed/blocked node that prevents targetID from running.
func (g *Graph) FirstBlocker(status map[string]Status, targetID string) (string, error) {
	if _, ok := g.nodes[targetID]; !ok {
		return "", fmt.Errorf("dag: unknown node %q", targetID)
	}
	for _, id := range g.order {
		if id == targetID {
			break
		}
		st := status[id]
		if st == StatusFailed || st == StatusBlocked {
			if g.reachable(id, targetID, map[string]bool{}) {
				return id, nil
			}
		}
	}
	return "", nil
}

func (g *Graph) reachable(from, to string, memo map[string]bool) bool {
	if from == to {
		return true
	}
	if v, ok := memo[from]; ok {
		return v
	}
	for _, ch := range g.childrenOf(from) {
		if g.reachable(ch, to, memo) {
			memo[from] = true
			return true
		}
	}
	memo[from] = false
	return false
}

func (g *Graph) childrenOf(id string) []string {
	var out []string
	for _, cand := range g.order {
		for _, in := range g.nodes[cand].Inputs {
			if in == id {
				out = append(out, cand)
			}
		}
	}
	sort.Strings(out)
	return out
}
