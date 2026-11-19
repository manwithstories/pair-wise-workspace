// Package track declares audio, video and derived tracks with explicit
// dependencies. A graph compiles to a deterministic execution order and
// rejects structural defects (duplicate nodes, missing dependencies, cycles,
// non-monotonic time segments) with locatable errors.
package track

import (
	"fmt"
	"sort"

	"avpipe/internal/clock"
	"avpipe/internal/fault"
)

// Kind classifies a track's role in the pipeline.
type Kind string

const (
	KindAudio   Kind = "audio"
	KindVideo   Kind = "video"
	KindDerived Kind = "derived"
)

// Node is one track. Spans describe the media intervals it covers; they must
// be ordered, non-negative and non-overlapping.
type Node struct {
	Name  string       `json:"name"`
	Kind  Kind         `json:"kind"`
	Deps  []string     `json:"deps,omitempty"`
	Spans []clock.Span `json:"spans,omitempty"`
}

// Graph is a mutable node collection keyed by track name.
type Graph struct {
	nodes map[string]*Node
	order []string
}

// New returns an empty graph.
func New() *Graph { return &Graph{nodes: map[string]*Node{}} }

// Add inserts a node, rejecting duplicates. Insertion order is retained for
// deterministic diagnostics.
func (g *Graph) Add(n Node) error {
	if n.Name == "" {
		return fault.New(fault.CodeInvalidArgument, "track.add", "node has empty name")
	}
	if _, ok := g.nodes[n.Name]; ok {
		return fault.New(fault.CodeGraphDuplicate, "track.add",
			"duplicate node %q", n.Name).WithTrack(n.Name)
	}
	switch n.Kind {
	case KindAudio, KindVideo, KindDerived:
	default:
		return fault.New(fault.CodeInvalidArgument, "track.add",
			"node %q has unknown kind %q", n.Name, n.Kind).WithTrack(n.Name)
	}
	cp := n
	cp.Deps = append([]string(nil), n.Deps...)
	cp.Spans = append([]clock.Span(nil), n.Spans...)
	g.nodes[n.Name] = &cp
	g.order = append(g.order, n.Name)
	return nil
}

// MustAdd is Add for tests and static graphs.
func (g *Graph) MustAdd(n Node) {
	if err := g.Add(n); err != nil {
		panic(err)
	}
}

// Node returns a node by name.
func (g *Graph) Node(name string) (Node, bool) {
	n, ok := g.nodes[name]
	if !ok {
		return Node{}, false
	}
	return *n, true
}

// Names lists nodes in insertion order.
func (g *Graph) Names() []string { return append([]string(nil), g.order...) }

// Validate checks structural integrity without computing an order: spans must
// be monotonic, dependencies must exist, and every derived/video node must
// declare at least one dependency when it claims to derive from something.
func (g *Graph) Validate() error {
	for _, name := range g.order {
		n := g.nodes[name]
		if e := clock.CheckMonotonic(n.Spans); e != nil {
			return fault.New(fault.CodeGraphMonotonic, "track.validate",
				"span %d [%d,%d) breaks ordering after [%d,%d)",
				e.Index, e.Cur.Start, e.Cur.End, e.Prev.Start, e.Prev.End).WithTrack(name)
		}
		for _, dep := range n.Deps {
			if dep == name {
				return fault.New(fault.CodeGraphCycle, "track.validate",
					"node %q depends on itself", name).WithTrack(name)
			}
			if _, ok := g.nodes[dep]; !ok {
				return fault.New(fault.CodeGraphMissing, "track.validate",
					"node %q depends on missing node %q", name, dep).WithTrack(name)
			}
		}
	}
	return nil
}

// Compile validates and returns a deterministic topological order. Nodes
// with no ordering relationship are sorted by name so repeated runs are
// identical. Dependencies always appear before their dependents.
func (g *Graph) Compile() ([]string, error) {
	if err := g.Validate(); err != nil {
		return nil, err
	}
	indeg := make(map[string]int, len(g.nodes))
	dependents := make(map[string][]string, len(g.nodes))
	for name, n := range g.nodes {
		indeg[name] += 0
		for _, dep := range n.Deps {
			indeg[name]++
			dependents[dep] = append(dependents[dep], name)
		}
	}
	var ready []string
	for name, d := range indeg {
		if d == 0 {
			ready = append(ready, name)
		}
	}
	sort.Strings(ready)
	order := make([]string, 0, len(g.nodes))
	for len(ready) > 0 {
		name := ready[0]
		ready = ready[1:]
		order = append(order, name)
		next := append([]string(nil), dependents[name]...)
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
		cycle := g.findCycle()
		return nil, fault.New(fault.CodeGraphCycle, "track.compile",
			"dependency cycle through %s", formatCycle(cycle))
	}
	return order, nil
}

// findCycle returns one concrete cycle for diagnostics, or nil.
func (g *Graph) findCycle() []string {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(g.nodes))
	var stack []string
	var cycle []string
	var visit func(name string) bool
	visit = func(name string) bool {
		color[name] = gray
		stack = append(stack, name)
		for _, dep := range g.nodes[name].Deps {
			switch color[dep] {
			case gray:
				start := 0
				for i, s := range stack {
					if s == dep {
						start = i
						break
					}
				}
				cycle = append(append([]string(nil), stack[start:]...), dep)
				return true
			case white:
				if visit(dep) {
					return true
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[name] = black
		return false
	}
	for _, name := range g.order {
		if color[name] == white && visit(name) {
			return cycle
		}
	}
	return nil
}

func formatCycle(cycle []string) string {
	if len(cycle) == 0 {
		return "<unknown>"
	}
	out := ""
	for i, s := range cycle {
		if i > 0 {
			out += " -> "
		}
		out += s
	}
	return out
}

// DerivedFrom returns the direct dependencies of a node.
func (g *Graph) DerivedFrom(name string) []string {
	if n, ok := g.nodes[name]; ok {
		return append([]string(nil), n.Deps...)
	}
	return nil
}

// DependentsOf returns the nodes that directly depend on name.
func (g *Graph) DependentsOf(name string) []string {
	var out []string
	for _, n := range g.nodes {
		for _, dep := range n.Deps {
			if dep == name {
				out = append(out, n.Name)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// String renders the graph as name(kind)<-deps for logs.
func (g *Graph) String() string {
	out := ""
	for i, name := range g.order {
		if i > 0 {
			out += "; "
		}
		n := g.nodes[name]
		out += fmt.Sprintf("%s(%s)", n.Name, n.Kind)
		if len(n.Deps) > 0 {
			out += " <- "
			for j, d := range n.Deps {
				if j > 0 {
					out += ","
				}
				out += d
			}
		}
	}
	return out
}
