// Package track models the explicit dependency graph between media tracks and
// normalizes channel layout, sample rate and color metadata in topological
// order so conflicts cannot propagate backward across dependent tracks.
package track

import (
	"fmt"
	"sort"

	"avrepair/internal/clock"
)

// Kind classifies a track.
type Kind string

const (
	KindAudio Kind = "audio"
	KindVideo Kind = "video"
	KindSub   Kind = "subtitle"
)

// Node is one vertex of the dependency graph: a track plus its declared
// format metadata and the edges it depends on.
type Node struct {
	ID         string
	Kind       Kind
	DependsOn  []string
	SampleRate int            // audio only, Hz
	Channels   int            // audio only
	Layout     string         // declared channel layout label
	Color      ColorMeta      // video only
	Payload    map[string]any // extension slot, kept deterministic in JSON
}

// ColorMeta is the source color metadata carried through the pipeline. Source
// is always preserved; conflicts are flagged, never silently rewritten.
type ColorMeta struct {
	Source string `json:"source"`
	Space  string `json:"space"`
	Range  string `json:"range"`
}

// Graph is an immutable-by-convention adjacency structure.
type Graph struct {
	nodes map[string]*Node
	edges map[string][]string // node -> nodes it depends on
}

// NewGraph validates references and constructs a graph. Unknown dependency
// targets are rejected up front.
func NewGraph(nodes []*Node) (*Graph, error) {
	g := &Graph{nodes: map[string]*Node{}, edges: map[string][]string{}}
	for _, n := range nodes {
		if n.ID == "" {
			return nil, clock.NewError(clock.CodeGraphCycle, "", clock.Interval{},
				"track node missing ID")
		}
		if _, dup := g.nodes[n.ID]; dup {
			return nil, clock.NewError(clock.CodeGraphCycle, n.ID, clock.Interval{},
				"duplicate track node ID")
		}
		g.nodes[n.ID] = n
	}
	for _, n := range nodes {
		for _, dep := range n.DependsOn {
			if _, ok := g.nodes[dep]; !ok {
				return nil, clock.NewError(clock.CodeGraphCycle, n.ID, clock.Interval{},
					fmt.Sprintf("dependency %q does not exist", dep))
			}
			g.edges[n.ID] = append(g.edges[n.ID], dep)
		}
	}
	return g, nil
}

// Topo returns node IDs in dependency order (dependencies before dependents)
// using Kahn's algorithm. Ties are broken by ID for deterministic output. A
// cycle yields a CodeGraphCycle error naming the unresolved nodes.
func (g *Graph) Topo() ([]string, error) {
	// indeg[id] = number of dependencies id still waits on.
	indeg := map[string]int{}
	for id := range g.nodes {
		indeg[id] = len(g.edges[id])
	}
	// Forward adjacency: dependency -> the nodes that depend on it.
	dependents := map[string][]string{}
	for id, deps := range g.edges {
		for _, d := range deps {
			dependents[d] = append(dependents[d], id)
		}
	}
	var ready []string
	for id, n := range indeg {
		if n == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	var order []string
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		next := append([]string{}, dependents[id]...)
		sort.Strings(next)
		for _, dep := range next {
			indeg[dep]--
			if indeg[dep] == 0 {
				ready = append(ready, dep)
				sort.Strings(ready)
			}
		}
	}
	if len(order) != len(g.nodes) {
		var unresolved []string
		for id, n := range indeg {
			if n > 0 {
				unresolved = append(unresolved, id)
			}
		}
		sort.Strings(unresolved)
		return nil, clock.NewError(clock.CodeGraphCycle, "", clock.Interval{},
			fmt.Sprintf("dependency cycle involving %v", unresolved))
	}
	return order, nil
}

// NodeByID looks up a vertex.
func (g *Graph) NodeByID(id string) (*Node, bool) {
	n, ok := g.nodes[id]
	return n, ok
}

// IDs returns all node IDs in sorted order.
func (g *Graph) IDs() []string {
	out := make([]string, 0, len(g.nodes))
	for id := range g.nodes {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
