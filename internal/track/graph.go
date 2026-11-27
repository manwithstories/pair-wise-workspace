// Package track models the dependency graph that orders pipeline work:
// source indexing feeds normalization, normalization feeds alignment and
// manifest publication. A stable topological order makes every run and
// every recovery deterministic.
package track

import (
	"sort"

	"fastpost/internal/model"
)

// NodeKind classifies a pipeline node.
type NodeKind string

const (
	KindSource    NodeKind = "source"
	KindNormalize NodeKind = "normalize"
	KindAlign     NodeKind = "align"
	KindManifest  NodeKind = "manifest"
)

// Node is one unit of pipeline work. Deps lists node IDs that must run
// first. A node may only depend on nodes that were added earlier.
type Node struct {
	ID   string
	Kind NodeKind
	Deps []string
}

// Graph is a directed acyclic graph keyed by node ID.
type Graph struct {
	order []string
	nodes map[string]*Node
	// edges[dep] = nodes that depend on dep (downstream fan-out).
	edges map[string][]string
}

// NewGraph creates an empty graph.
func NewGraph() *Graph {
	return &Graph{nodes: map[string]*Node{}, edges: map[string][]string{}}
}

// Add registers a node. Duplicate IDs and unknown dependencies fail fast.
func (g *Graph) Add(n Node) error {
	if n.ID == "" {
		return model.Fault{Kind: model.FaultGraphCycle, Track: n.ID, Offset: -1,
			Reason: "node id is empty"}
	}
	if _, exists := g.nodes[n.ID]; exists {
		return model.Fault{Kind: model.FaultGraphCycle, Track: n.ID, Offset: -1,
			Reason: "duplicate node id"}
	}
	for _, d := range n.Deps {
		if _, ok := g.nodes[d]; !ok {
			return model.Fault{Kind: model.FaultGraphCycle, Track: n.ID, Offset: -1,
				Reason: "unknown dependency " + d}
		}
	}
	cp := Node{ID: n.ID, Kind: n.Kind, Deps: append([]string(nil), n.Deps...)}
	g.nodes[n.ID] = &cp
	g.order = append(g.order, n.ID)
	for _, d := range n.Deps {
		g.edges[d] = append(g.edges[d], n.ID)
	}
	return nil
}

// Topo returns node IDs in a stable topological order: nodes whose
// remaining dependencies are all satisfied are emitted in insertion
// order (Kahn's algorithm with the registration order as the queue).
// If the graph contains a cycle, the nodes still blocked are returned
// together with a structured fault and nothing downstream is written.
func (g *Graph) Topo() ([]string, error) {
	indeg := make(map[string]int, len(g.order))
	for _, id := range g.order {
		indeg[id] = len(g.nodes[id].Deps)
	}
	// Queue preserves registration order for determinism.
	var ready []string
	for _, id := range g.order {
		if indeg[id] == 0 {
			ready = append(ready, id)
		}
	}
	out := make([]string, 0, len(g.order))
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		out = append(out, id)
		// Downstream edges were appended in registration order; keep that
		// order stable by iterating as stored.
		for _, next := range g.edges[id] {
			indeg[next]--
			if indeg[next] == 0 {
				ready = append(ready, next)
			}
		}
	}
	if len(out) != len(g.order) {
		blocked := g.blocked(indeg)
		return nil, model.Fault{
			Kind: model.FaultGraphCycle, Track: blocked[0], Offset: -1,
			Reason: "dependency cycle involving: " + joinSorted(blocked),
		}
	}
	return out, nil
}

func (g *Graph) blocked(indeg map[string]int) []string {
	var b []string
	for _, id := range g.order {
		if indeg[id] > 0 {
			b = append(b, id)
		}
	}
	sort.Strings(b)
	return b
}

// Downstream returns the node itself plus every transitive dependent,
// in stable topological order. Recovery reruns exactly this set when a
// source node's input digest changes.
func (g *Graph) Downstream(id string) ([]string, bool) {
	if _, ok := g.nodes[id]; !ok {
		return nil, false
	}
	seen := map[string]bool{id: true}
	frontier := []string{id}
	for len(frontier) > 0 {
		cur := frontier[0]
		frontier = frontier[1:]
		for _, next := range g.edges[cur] {
			if !seen[next] {
				seen[next] = true
				frontier = append(frontier, next)
			}
		}
	}
	out := make([]string, 0, len(seen))
	topo, err := g.Topo()
	if err != nil {
		// Cyclic graph: fall back to registration order for the subset.
		for _, nid := range g.order {
			if seen[nid] {
				out = append(out, nid)
			}
		}
		return out, true
	}
	for _, nid := range topo {
		if seen[nid] {
			out = append(out, nid)
		}
	}
	return out, true
}

// Node looks a node up by ID.
func (g *Graph) Node(id string) (Node, bool) {
	n, ok := g.nodes[id]
	if !ok {
		return Node{}, false
	}
	return *n, true
}

// Size returns the registered node count.
func (g *Graph) Size() int { return len(g.order) }

func joinSorted(ids []string) string {
	cp := append([]string(nil), ids...)
	sort.Strings(cp)
	out := ""
	for i, id := range cp {
		if i > 0 {
			out += ","
		}
		out += id
	}
	return out
}
