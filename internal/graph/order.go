// Package graph models dependencies between media tracks and processing
// stages. The two domain rules encoded here are: a video track depends on its
// companion audio track for alignment, and index construction depends on
// timestamp repair finishing first. The graph yields deterministic parallel
// processing layers and reports the exact loop when a cycle is present.
package graph

import (
	"fmt"
	"sort"

	"github.com/mediamend/mediamend/internal/model"
)

// Kind classifies a graph node.
type Kind string

const (
	KindAudio Kind = "audio"
	KindVideo Kind = "video"
	KindIndex Kind = "index"
)

// Graph is a directed dependency graph: an edge from -> to means "from
// requires to to be processed first".
type Graph struct {
	kind  map[string]Kind
	edges map[string]map[string]struct{} // from -> set of prerequisites
}

// New creates an empty graph.
func New() *Graph {
	return &Graph{kind: map[string]Kind{}, edges: map[string]map[string]struct{}{}}
}

// AddTrack registers a track. Re-adding the same id with the same kind is a
// no-op; a conflicting kind is an error.
func (g *Graph) AddTrack(id string, kind Kind) error {
	if id == "" {
		return fmt.Errorf("graph: empty track id")
	}
	if k, ok := g.kind[id]; ok && k != kind {
		return fmt.Errorf("graph: track %s re-declared as %s, was %s", id, kind, k)
	}
	g.kind[id] = kind
	if g.edges[id] == nil {
		g.edges[id] = map[string]struct{}{}
	}
	return nil
}

// AddDependency records that from requires prerequisite. Unknown endpoints
// are rejected so a typo cannot silently create a dangling edge.
func (g *Graph) AddDependency(from, prerequisite string) error {
	if _, ok := g.kind[from]; !ok {
		return fmt.Errorf("graph: unknown track %q", from)
	}
	if _, ok := g.kind[prerequisite]; !ok {
		return fmt.Errorf("graph: unknown prerequisite %q", prerequisite)
	}
	g.edges[from][prerequisite] = struct{}{}
	return nil
}

// Order returns processing layers: every node in layer n has all its
// prerequisites in earlier layers, so each layer can run concurrently. Node
// order inside a layer is alphabetical, giving deterministic scheduling. When
// a cycle exists no layers are returned; instead the cycle path is reported
// together with a graph_cycle fault.
func (g *Graph) Order(file string) ([][]string, []string, []model.Fault, error) {
	indeg := map[string]int{}
	for id := range g.kind {
		indeg[id] = len(g.edges[id])
	}
	// reverse adjacency: prerequisite -> dependents
	dependents := map[string][]string{}
	for from, prereqs := range g.edges {
		for p := range prereqs {
			dependents[p] = append(dependents[p], from)
		}
	}
	for k := range dependents {
		sort.Strings(dependents[k])
	}

	var layers [][]string
	processed := map[string]bool{}
	for {
		var layer []string
		for id := range g.kind {
			if !processed[id] && indeg[id] == 0 {
				layer = append(layer, id)
			}
		}
		if len(layer) == 0 {
			break
		}
		sort.Strings(layer)
		layers = append(layers, layer)
		for _, id := range layer {
			processed[id] = true
			for _, dep := range dependents[id] {
				indeg[dep]--
			}
		}
	}

	if len(processed) != len(g.kind) {
		cycle := g.findCycle()
		f := model.Fault{
			Class: model.FaultGraphCycle, Severity: model.SeverityFatal,
			Location: model.SourceLocation{File: file, Track: firstOrEmpty(cycle)},
			Message:  fmt.Sprintf("dependency cycle detected: %s", formatCycle(cycle)),
			Actual:   formatCycle(cycle),
		}
		return nil, cycle, []model.Fault{f}, &model.FaultError{Faults: []model.Fault{f}}
	}
	return layers, nil, nil, nil
}

// findCycle returns one cycle as an ordered path whose first and last node
// are identical, using DFS three-color marking.
func (g *Graph) findCycle() []string {
	const white, gray, black = 0, 1, 2
	color := map[string]int{}
	var stack []string
	var dfs func(string) []string
	ids := make([]string, 0, len(g.kind))
	for id := range g.kind {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	dfs = func(u string) []string {
		color[u] = gray
		stack = append(stack, u)
		prereqs := make([]string, 0, len(g.edges[u]))
		for p := range g.edges[u] {
			prereqs = append(prereqs, p)
		}
		sort.Strings(prereqs)
		for _, v := range prereqs {
			switch color[v] {
			case white:
				if cyc := dfs(v); cyc != nil {
					return cyc
				}
			case gray:
				i := indexOf(stack, v)
				return append(append([]string{}, stack[i:]...), v)
			}
		}
		stack = stack[:len(stack)-1]
		color[u] = black
		return nil
	}
	for _, id := range ids {
		if color[id] == white {
			if cyc := dfs(id); cyc != nil {
				return cyc
			}
		}
	}
	return nil
}

// BlockedBy returns every track that transitively depends on any of failed,
// i.e. the tracks that must be marked incomplete downstream of a failure.
func (g *Graph) BlockedBy(failed []string) []string {
	blocked := map[string]struct{}{}
	queue := append([]string(nil), failed...)
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for from, prereqs := range g.edges {
			if _, isBlocked := blocked[from]; isBlocked || contains(failed, from) {
				continue
			}
			if _, needs := prereqs[cur]; needs {
				blocked[from] = struct{}{}
				queue = append(queue, from)
			}
		}
	}
	out := make([]string, 0, len(blocked))
	for id := range blocked {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

func contains(s []string, v string) bool { return indexOf(s, v) >= 0 }

func firstOrEmpty(s []string) string {
	if len(s) > 0 {
		return s[0]
	}
	return ""
}

func formatCycle(cycle []string) string {
	out := ""
	for i, n := range cycle {
		if i > 0 {
			out += " -> "
		}
		out += n
	}
	return out
}
