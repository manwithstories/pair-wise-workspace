// Package timeline builds the track dependency DAG used to order compositing,
// masking and offsets. Cycles must fail before any output is written.
package timeline

import (
	"errors"
	"fmt"
	"sort"

	"fieldcut/internal/media"
)

// Rect is an inclusive-exclusive pixel region used as a compositing mask.
type Rect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

// Intersect returns the overlap of two rectangles.
func (r Rect) Intersect(o Rect) Rect {
	x0 := maxInt(r.X, o.X)
	y0 := maxInt(r.Y, o.Y)
	x1 := minInt(r.X+r.W, o.X+o.W)
	y1 := minInt(r.Y+r.H, o.Y+o.H)
	if x1 <= x0 || y1 <= y0 {
		return Rect{}
	}
	return Rect{X: x0, Y: y0, W: x1 - x0, H: y1 - y0}
}

func (r Rect) Empty() bool { return r.W <= 0 || r.H <= 0 }
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Node is one track in the DAG.
type Node struct {
	Name     string
	Offset   media.Rational // time offset applied to every frame on the track
	Priority int            // higher wins in overlap regions
	Mask     Rect           // visible region; zero means full frame
	Deps     []string       // tracks that must composite first
}

// Graph is a validated DAG with a deterministic topological order.
type Graph struct {
	nodes map[string]Node
	order []string // dependencies first
}

// Build validates unique names, existing dependencies, priority bounds and
// acyclicity. A cycle is reported with the participating track names.
func Build(nodes []Node) (*Graph, error) {
	if len(nodes) == 0 {
		return nil, media.New(media.KindGraph, "graph.build", "", -1, errors.New("no tracks"))
	}
	if len(nodes) > media.MaxTracks {
		return nil, media.Newf(media.KindLimit, "graph.build", "", -1, "track count %d > %d", len(nodes), media.MaxTracks)
	}
	g := &Graph{nodes: make(map[string]Node, len(nodes))}
	for _, n := range nodes {
		if n.Name == "" {
			return nil, media.New(media.KindGraph, "graph.build", "", -1, errors.New("empty track name"))
		}
		if _, dup := g.nodes[n.Name]; dup {
			return nil, media.Newf(media.KindGraph, "graph.build", n.Name, -1, "duplicate track")
		}
		if n.Priority < 0 || n.Priority > 100 {
			return nil, media.Newf(media.KindGraph, "graph.build", n.Name, -1, "priority %d outside [0,100]", n.Priority)
		}
		if n.Offset == (media.Rational{}) {
			n.Offset = media.NewRational(0, 1) // zero value means "no offset"
		}
		if n.Offset.Den == 0 || n.Offset.Num == 0 && n.Offset.Den != 1 {
			return nil, media.Newf(media.KindGraph, "graph.build", n.Name, -1, "malformed offset %s", n.Offset)
		}
		if !n.Mask.Empty() && (n.Mask.X < 0 || n.Mask.Y < 0) {
			return nil, media.Newf(media.KindGraph, "graph.build", n.Name, -1, "negative mask origin")
		}
		cp := n
		cp.Deps = append([]string(nil), n.Deps...)
		g.nodes[n.Name] = cp
	}
	for name, n := range g.nodes {
		for _, d := range n.Deps {
			if d == name {
				return nil, media.Newf(media.KindGraph, "graph.cycle", name, -1, "track depends on itself")
			}
			if _, ok := g.nodes[d]; !ok {
				return nil, media.Newf(media.KindGraph, "graph.deps", name, -1, "unknown dependency %q", d)
			}
		}
	}
	order, err := g.topological()
	if err != nil {
		return nil, err
	}
	g.order = order
	return g, nil
}

// topological runs Kahn's algorithm with a deterministic tie-break by
// priority (descending) then name, so equal inputs always composite alike.
func (g *Graph) topological() ([]string, error) {
	indeg := make(map[string]int, len(g.nodes))
	dependents := make(map[string][]string, len(g.nodes))
	for name, n := range g.nodes {
		indeg[name] = len(n.Deps)
		for _, d := range n.Deps {
			dependents[d] = append(dependents[d], name)
		}
	}
	for _, list := range dependents {
		sort.Strings(list)
	}
	ready := make([]string, 0, len(g.nodes))
	for name, d := range indeg {
		if d == 0 {
			ready = append(ready, name)
		}
	}
	less := func(a, b string) bool {
		pa, pb := g.nodes[a].Priority, g.nodes[b].Priority
		if pa != pb {
			return pa > pb
		}
		return a < b
	}
	sort.Slice(ready, func(i, j int) bool { return less(ready[i], ready[j]) })
	order := make([]string, 0, len(g.nodes))
	for len(ready) > 0 {
		name := ready[0]
		ready = ready[1:]
		order = append(order, name)
		for _, dep := range dependents[name] {
			indeg[dep]--
			if indeg[dep] == 0 {
				ready = append(ready, dep)
			}
		}
		sort.Slice(ready, func(i, j int) bool { return less(ready[i], ready[j]) })
	}
	if len(order) != len(g.nodes) {
		var stuck []string
		for name, d := range indeg {
			if d > 0 {
				stuck = append(stuck, name)
			}
		}
		sort.Strings(stuck)
		return nil, media.Newf(media.KindGraph, "graph.cycle", "", -1,
			"dependency cycle among tracks %v", stuck)
	}
	return order, nil
}

// Order returns dependencies-first compositing order.
func (g *Graph) Order() []string { return append([]string(nil), g.order...) }

// Node returns a copy of the named track.
func (g *Graph) Node(name string) (Node, bool) {
	n, ok := g.nodes[name]
	return n, ok
}

// CompositeOrder returns the order in which frames should be painted:
// dependencies first, then ascending priority, with name as tie-break.
func (g *Graph) CompositeOrder() []string {
	rank := make(map[string]int, len(g.order))
	for i, name := range g.order {
		rank[name] = i
	}
	out := g.Order()
	sort.SliceStable(out, func(i, j int) bool {
		pi, pj := g.nodes[out[i]].Priority, g.nodes[out[j]].Priority
		if pi != pj {
			return pi < pj
		}
		return rank[out[i]] < rank[out[j]]
	})
	return out
}

// String renders the plan for terminal output.
func (g *Graph) String() string {
	s := "tracks:"
	for _, name := range g.order {
		n := g.nodes[name]
		s += fmt.Sprintf(" %s(p=%d,off=%s,mask=%dx%d)@%dx%d", name, n.Priority, n.Offset, n.Mask.W, n.Mask.H, n.Mask.X, n.Mask.Y)
	}
	return s
}
