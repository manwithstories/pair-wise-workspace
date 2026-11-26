// Package timeline builds the explicit per-track dependency graph, orders
// tracks via topological sort and repairs mfhd sequence gaps and tfdt
// rollback onto one shared monotonic timeline.
package timeline

import (
	"sort"

	"relay/internal/media"
)

// Node is one track vertex together with its declared synchronization
// dependencies (edges point from a track to every track it waits on).
type Node struct {
	Spec      TrackSpec
	Published bool // false when the track failed validation or was blocked
	Reason    *media.Fault
}

// Graph is the explicit directed dependency graph of all tracks.
type Graph struct {
	nodes map[uint32]*Node
	order []uint32 // deterministic topological order
	// cycleRejected counts dependency cycles; a self-loop and a multi-node
	// cycle each count as one rejected component.
	cycleRejected int
}

// NewGraph constructs a graph from track specs, validates references and
// computes the topological processing order. Missing dependencies, cycles
// and the resulting blocked sets are recorded as faults but never panic.
func NewGraph(specs []TrackSpec) *Graph {
	g := &Graph{nodes: make(map[uint32]*Node, len(specs))}
	for _, s := range specs {
		g.nodes[s.ID] = &Node{Spec: s}
	}
	// Deterministic edge order keeps every downstream result reproducible.
	ids := make([]uint32, 0, len(specs))
	for id := range g.nodes {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	indeg := make(map[uint32]int, len(ids))
	adj := make(map[uint32][]uint32)
	for _, id := range ids {
		n := g.nodes[id]
		for _, dep := range n.Spec.DependsOn {
			if _, ok := g.nodes[dep]; !ok {
				n.Reason = media.NewFault(media.FaultMissingDep, id, 0, 0,
					"declared dependency track does not exist")
				continue
			}
			adj[dep] = append(adj[dep], id)
			indeg[id]++
		}
	}

	// Kahn's algorithm with a min-heap-ish ready list for deterministic order.
	ready := make([]uint32, 0)
	for _, id := range ids {
		if indeg[id] == 0 {
			ready = append(ready, id)
		}
	}
	visited := make(map[uint32]bool, len(ids))
	for len(ready) > 0 {
		sort.Slice(ready, func(i, j int) bool { return ready[i] < ready[j] })
		id := ready[0]
		ready = ready[1:]
		if visited[id] {
			continue
		}
		visited[id] = true
		g.order = append(g.order, id)
		for _, next := range adj[id] {
			indeg[next]--
			if indeg[next] == 0 {
				ready = append(ready, next)
			}
		}
	}

	// Every unvisited vertex participates in a cycle. Group them into
	// connected cycle components by treating undirected adjacency.
	cycleSet := map[uint32]bool{}
	for _, id := range ids {
		if !visited[id] {
			cycleSet[id] = true
		}
	}
	if len(cycleSet) > 0 {
		g.cycleRejected = countComponents(cycleSet, g.nodes)
		for id := range cycleSet {
			g.nodes[id].Reason = media.NewFault(media.FaultGraphCycle, id, 0, 0,
				"track belongs to a dependency cycle")
		}
	}
	return g
}

// countComponents counts weakly connected components restricted to members.
func countComponents(members map[uint32]bool, nodes map[uint32]*Node) int {
	seen := map[uint32]bool{}
	components := 0
	var start uint32
	for id := range members {
		start = id
		break
	}
	_ = start
	for root := range members {
		if seen[root] {
			continue
		}
		components++
		queue := []uint32{root}
		seen[root] = true
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			n := nodes[cur]
			neighbors := append([]uint32(nil), n.Spec.DependsOn...)
			for _, dep := range n.Spec.DependsOn {
				for _, back := range nodes[dep].Spec.DependsOn {
					neighbors = append(neighbors, back)
				}
			}
			for _, nb := range neighbors {
				if members[nb] && !seen[nb] {
					seen[nb] = true
					queue = append(queue, nb)
				}
			}
		}
	}
	return components
}

// Order returns the topological track processing order. Cycle members are
// excluded because they can never be scheduled safely.
func (g *Graph) Order() []uint32 { return append([]uint32(nil), g.order...) }

// Node looks up a track node.
func (g *Graph) Node(id uint32) (*Node, bool) {
	n, ok := g.nodes[id]
	return n, ok
}

// CycleRejected reports how many cyclic components were rejected.
func (g *Graph) CycleRejected() int { return g.cycleRejected }

// PropagateFailures marks every track whose dependency chain contains a
// non-published track as blocked, while independent tracks keep processing.
func (g *Graph) PropagateFailures() {
	for _, id := range g.order {
		n := g.nodes[id]
		if n.Reason != nil {
			continue
		}
		for _, dep := range n.Spec.DependsOn {
			d, ok := g.nodes[dep]
			if !ok || !d.Published || d.Reason != nil {
				n.Reason = media.NewFault(media.FaultDepFailed, id, 0, 0,
					"dependency track failed or was not published")
				break
			}
		}
	}
}

// MarkPublished records the outcome of repairing one track.
func (g *Graph) MarkPublished(id uint32, fault *media.Fault) {
	n, ok := g.nodes[id]
	if !ok {
		return
	}
	if fault != nil {
		n.Reason = fault
		n.Published = false
		return
	}
	n.Published = true
}
