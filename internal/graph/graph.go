// Package graph models the archive timeline as a directed dependency graph:
// tracks depend on other tracks (a proxy on its master, audio on video), and
// clips depend on the clip they are cut from plus the clips a crossfade joins.
package graph

import (
	"fmt"
)

// NodeKind distinguishes the two node species.
type NodeKind string

const (
	NodeTrack NodeKind = "track"
	NodeClip  NodeKind = "clip"
)

// Node is one vertex.
type Node struct {
	ID    string
	Kind  NodeKind
	Track string // owning track, for clips
	Label string
	Size  int // payload size hint used for planning only
}

// Edge is one directed dependency: From must complete before To may start.
type Edge struct {
	From, To string
	Reason   string
}

// Graph is an immutable vertex/edge set once built.
type Graph struct {
	nodes map[string]*Node
	order []string // insertion order, for deterministic iteration
	out   map[string][]Edge
	in    map[string][]Edge
}

// New returns an empty graph.
func New() *Graph {
	return &Graph{
		nodes: map[string]*Node{},
		out:   map[string][]Edge{},
		in:    map[string][]Edge{},
	}
}

// AddNode inserts a node, returning the existing one on duplicate ID.
func (g *Graph) AddNode(n Node) *Node {
	if cur, ok := g.nodes[n.ID]; ok {
		return cur
	}
	cp := n
	g.nodes[n.ID] = &cp
	g.order = append(g.order, n.ID)
	return &cp
}

// AddEdge records a dependency and reports a duplicate insert.
func (g *Graph) AddEdge(e Edge) bool {
	if _, ok := g.nodes[e.From]; !ok {
		return false
	}
	if _, ok := g.nodes[e.To]; !ok {
		return false
	}
	for _, cur := range g.out[e.From] {
		if cur.To == e.To {
			return false
		}
	}
	g.out[e.From] = append(g.out[e.From], e)
	g.in[e.To] = append(g.in[e.To], e)
	return true
}

// Node returns a vertex by ID.
func (g *Graph) Node(id string) (*Node, bool) {
	n, ok := g.nodes[id]
	return n, ok
}

// Nodes returns vertices in insertion order.
func (g *Graph) Nodes() []*Node {
	out := make([]*Node, 0, len(g.order))
	for _, id := range g.order {
		out = append(out, g.nodes[id])
	}
	return out
}

// Edges returns every edge in insertion order of their source.
func (g *Graph) Edges() []Edge {
	var out []Edge
	for _, id := range g.order {
		out = append(out, g.out[id]...)
	}
	return out
}

// Dependents returns the edges leaving a node.
func (g *Graph) Dependents(id string) []Edge { return g.out[id] }

// Dependencies returns the edges entering a node.
func (g *Graph) Dependencies(id string) []Edge { return g.in[id] }

// TopoOrder returns nodes in dependency-first order. Nodes inside a cycle are
// excluded; the second return value lists their IDs.
func (g *Graph) TopoOrder() ([]string, []string) {
	indeg := map[string]int{}
	for _, id := range g.order {
		indeg[id] = 0
	}
	for _, e := range g.Edges() {
		indeg[e.To]++
	}
	// Insertion order is already canonical (tracks, then clips, then
	// transitions), and a FIFO queue preserves it without repeated sorting.
	var ready []string
	for _, id := range g.order {
		if indeg[id] == 0 {
			ready = append(ready, id)
		}
	}
	var order []string
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		for _, e := range g.out[id] {
			indeg[e.To]--
			if indeg[e.To] == 0 {
				ready = append(ready, e.To)
			}
		}
	}
	var stuck []string
	for _, id := range g.order {
		if indeg[id] > 0 {
			stuck = append(stuck, id)
		}
	}
	return order, stuck
}

// DependentClosure returns every node that transitively depends on id.
func (g *Graph) DependentClosure(id string) []string {
	seen := map[string]bool{}
	var out []string
	var visit func(cur string)
	visit = func(cur string) {
		for _, e := range g.out[cur] {
			if seen[e.To] {
				continue
			}
			seen[e.To] = true
			out = append(out, e.To)
			visit(e.To)
		}
	}
	visit(id)
	return out
}

// Summary renders a compact one-line description for the terminal report.
func (g *Graph) Summary() string {
	nodes, edges := len(g.order), 0
	for _, id := range g.order {
		edges += len(g.out[id])
	}
	return fmt.Sprintf("nodes=%d edges=%d", nodes, edges)
}
