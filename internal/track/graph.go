// Package track assembles the dependency DAG for the tracks in a fragment
// group. Recovery runs in topological order so that a track only costs
// re-fetch budget after its prerequisites are resolved. Cycles are rejected.
package track

import (
	"sort"

	"pairwise/ingestcheck/internal/clock"
)

// Kind classifies a track's role in the pair.
type Kind string

const (
	Video Kind = "video"
	Audio Kind = "audio"
)

// Node is one track plus the tracks whose recovery it depends on.
type Node struct {
	ID       uint32
	Kind     Kind
	Name     string
	Rate     int64 // ticks per second
	Depends  []uint32
	NeededBy []uint32
}

// Graph is an immutable DAG. Build validates uniqueness and acyclicity and
// then serves the recovery order.
type Graph struct {
	nodes map[uint32]*Node
	order []uint32
}

// Build validates nodes and computes a stable topological order. Dependencies
// that reference unknown tracks are rejected; cycles are rejected with the
// offending track ID.
func Build(nodes []Node) (*Graph, error) {
	g := &Graph{nodes: make(map[uint32]*Node, len(nodes))}
	ids := make([]uint32, 0, len(nodes))
	for i := range nodes {
		n := nodes[i]
		if n.ID == 0 {
			return nil, clock.New(clock.CodeTrackMissing, "track", "zero track id")
		}
		if n.Kind != Video && n.Kind != Audio {
			return nil, clock.New(clock.CodeTrackMissing, "track", "unknown kind for track "+n.Name)
		}
		if _, dup := g.nodes[n.ID]; dup {
			return nil, clock.New(clock.CodeTrackMissing, "track", "duplicate track id")
		}
		cp := n
		cp.Depends = append([]uint32(nil), n.Depends...)
		g.nodes[n.ID] = &cp
		ids = append(ids, n.ID)
	}
	// Resolve edges, rejecting dangling dependencies.
	for _, id := range ids {
		n := g.nodes[id]
		for _, dep := range n.Depends {
			d, ok := g.nodes[dep]
			if !ok {
				return nil, clock.New(clock.CodeTrackMissing, "track", "unknown dependency")
			}
			d.NeededBy = append(d.NeededBy, id)
		}
	}
	// Deterministic Kahn ordering: lowest ready ID first.
	indeg := make(map[uint32]int, len(nodes))
	for _, id := range ids {
		indeg[id] = len(g.nodes[id].Depends)
	}
	ready := make([]uint32, 0, len(nodes))
	for _, id := range ids {
		if indeg[id] == 0 {
			ready = append(ready, id)
		}
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i] < ready[j] })
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		g.order = append(g.order, id)
		next := g.nodes[id].NeededBy
		for _, nb := range next {
			indeg[nb]--
			if indeg[nb] == 0 {
				ready = append(ready, nb)
			}
		}
		sort.Slice(ready, func(i, j int) bool { return ready[i] < ready[j] })
	}
	if len(g.order) != len(nodes) {
		// A node remains with a positive in-degree, so the graph has a cycle.
		for _, id := range ids {
			if indeg[id] > 0 {
				return nil, clock.New(clock.CodeTrackCycle, "track", "cycle through track "+g.nodes[id].Name)
			}
		}
		return nil, clock.New(clock.CodeTrackCycle, "track", "cycle detected")
	}
	return g, nil
}

// Order returns the topological recovery order, dependencies first.
func (g *Graph) Order() []uint32 { return append([]uint32(nil), g.order...) }

// Node returns the node for an ID.
func (g *Graph) Node(id uint32) (*Node, bool) {
	n, ok := g.nodes[id]
	return n, ok
}

// Len reports the number of tracks.
func (g *Graph) Len() int { return len(g.nodes) }

// Antecedents returns the transitive dependencies of id in recovery order.
func (g *Graph) Antecedents(id uint32) []uint32 {
	var out []uint32
	seen := map[uint32]bool{}
	var visit func(uint32)
	visit = func(cur uint32) {
		for _, dep := range g.nodes[cur].Depends {
			if seen[dep] {
				continue
			}
			seen[dep] = true
			visit(dep)
			out = append(out, dep)
		}
	}
	if _, ok := g.nodes[id]; ok {
		visit(id)
	}
	return out
}

// KindOf returns the kind of a track, defaulting to Video for unknown ids so
// callers can key matching on it without a second lookup.
func (g *Graph) KindOf(id uint32) Kind {
	if n, ok := g.nodes[id]; ok {
		return n.Kind
	}
	return Video
}

// DemoGraph builds the two-track pair used by the sample: the video track
// gates audio recovery because the audio timeline is anchored to the first
// decodable video frame.
func DemoGraph() (*Graph, error) {
	return Build([]Node{
		{ID: 1, Kind: Video, Name: "v1", Rate: 90000},
		{ID: 2, Kind: Audio, Name: "a1", Rate: 48000, Depends: []uint32{1}},
	})
}
