package graph

import (
	"sort"

	"mediasim/internal/clock"
)

// topoOrder runs Kahn's algorithm with a deterministic ready queue: among
// ready nodes, the one earliest on the timeline (then track, then ID) runs
// first. This is the "stable handling of same-instant segments" rule.
func (g *Graph) topoOrder() ([]NodeID, error) {
	indeg := make(map[NodeID]int, len(g.nodes))
	children := make(map[NodeID][]NodeID, len(g.nodes))
	for _, n := range g.nodes {
		if _, ok := indeg[n.ID]; !ok {
			indeg[n.ID] = 0
		}
		for _, d := range n.Deps {
			if _, ok := g.byID[d]; !ok {
				return nil, invalid(clock.CatGraph, n.Span(),
					"node %s depends on missing node %s", n.ID, d)
			}
			indeg[n.ID]++
			children[d] = append(children[d], n.ID)
		}
	}

	ready := make([]NodeID, 0, len(g.nodes))
	for _, n := range g.nodes {
		if indeg[n.ID] == 0 {
			ready = append(ready, n.ID)
		}
	}
	g.sortReady(ready)

	order := make([]NodeID, 0, len(g.nodes))
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		for _, c := range children[id] {
			indeg[c]--
			if indeg[c] == 0 {
				ready = append(ready, c)
			}
		}
		g.sortReady(ready)
	}

	if len(order) != len(g.nodes) {
		// Cycle: report the remaining nodes at their earliest position.
		remaining := make([]Node, 0, len(g.nodes)-len(order))
		done := make(map[NodeID]struct{}, len(order))
		for _, id := range order {
			done[id] = struct{}{}
		}
		for _, n := range g.nodes {
			if _, ok := done[n.ID]; !ok {
				remaining = append(remaining, n)
			}
		}
		return nil, g.cycleError(remaining, children)
	}
	return order, nil
}

func (g *Graph) sortReady(ids []NodeID) {
	sort.SliceStable(ids, func(i, j int) bool {
		a, _ := g.Node(ids[i])
		b, _ := g.Node(ids[j])
		if a.Start != b.Start {
			return a.Start < b.Start
		}
		// At the same instant, a segment's own work precedes the pieces it
		// produced, so a segment and its first piece stay adjacent.
		if a.Kind != b.Kind {
			return a.Kind == NodeSegment
		}
		if a.Track != b.Track {
			return a.Track < b.Track
		}
		return a.ID < b.ID
	})
}

// cycleError names one concrete cycle so the message is reproducible.
func (g *Graph) cycleError(remaining []Node, children map[NodeID][]NodeID) error {
	stay := make(map[NodeID]struct{}, len(remaining))
	for _, n := range remaining {
		stay[n.ID] = struct{}{}
	}
	start := remaining[0].ID
	// Walk forward inside the unresolved set until a node repeats.
	path := []NodeID{start}
	seen := map[NodeID]int{start: 0}
	cur := start
	for {
		var next NodeID
		for _, c := range children[cur] {
			if _, ok := stay[c]; ok {
				next = c
				break
			}
		}
		if next == "" {
			break
		}
		if at, ok := seen[next]; ok {
			path = append(path, path[at:]...)
			break
		}
		seen[next] = len(path)
		path = append(path, next)
		cur = next
	}
	node, _ := g.Node(path[0])
	chain := ""
	for i, id := range path {
		if i > 0 {
			chain += " -> "
		}
		chain += string(id)
	}
	return invalid(clock.CatGraph, node.Span(), "dependency cycle: %s", chain)
}
