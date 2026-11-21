package track

import (
	"fmt"

	"mkvarchive/internal/model"
)

// Graph is the directed dependency graph of source tracks. An edge t -> d
// means track d must be planned and written before track t (t declares a
// TrackJoinUID pointing at d).
type Graph struct {
	Tracks []*model.Track
	byUID  map[uint64]*model.Track
	edges  map[uint64][]uint64
}

// BuildGraph validates declarations and constructs the dependency graph.
// Validation order is fixed so the first error is deterministic:
// duplicate TrackUID, missing JoinUID target, then dependency cycle.
func BuildGraph(tracks []*model.Track) (*Graph, error) {
	g := &Graph{
		Tracks: tracks,
		byUID:  make(map[uint64]*model.Track, len(tracks)),
		edges:  make(map[uint64][]uint64),
	}
	seen := map[uint64]bool{}
	for _, tr := range tracks {
		if seen[tr.UID] {
			return nil, model.NewError("TRACK_DUPLICATE_UID",
				fmt.Sprintf("duplicate TrackUID %d", tr.UID), tr.HeaderOffset)
		}
		seen[tr.UID] = true
		g.byUID[tr.UID] = tr
	}
	for _, tr := range tracks {
		if tr.JoinUID <= 0 {
			continue
		}
		dep, ok := g.byUID[uint64(tr.JoinUID)]
		if !ok {
			return nil, model.NewError("TRACK_MISSING_DEPENDENCY",
				fmt.Sprintf("track %d joins unknown UID %d", tr.UID, tr.JoinUID),
				tr.HeaderOffset)
		}
		g.edges[tr.UID] = append(g.edges[tr.UID], dep.UID)
	}
	if err := g.detectCycle(); err != nil {
		return nil, err
	}
	return g, nil
}

// detectCycle runs Kahn's algorithm; whatever remains after draining the
// queue belongs to a cycle. The error is anchored at the earliest-offset
// track in the surviving set, giving a stable "first error position".
func (g *Graph) detectCycle() error {
	indegree := make(map[uint64]int, len(g.Tracks))
	for _, tr := range g.Tracks {
		indegree[tr.UID] = 0
	}
	for _, deps := range g.edges {
		for _, d := range deps {
			indegree[d]++
		}
	}
	// Stable queue: declaration order so results are deterministic.
	var queue []uint64
	for _, tr := range g.Tracks {
		if indegree[tr.UID] == 0 {
			queue = append(queue, tr.UID)
		}
	}
	visited := 0
	for head := 0; head < len(queue); head++ {
		uid := queue[head]
		visited++
		for _, dep := range g.edges[uid] {
			indegree[dep]--
			if indegree[dep] == 0 {
				queue = append(queue, dep)
			}
		}
	}
	if visited == len(g.Tracks) {
		return nil
	}
	var first *model.Track
	for _, tr := range g.Tracks {
		if indegree[tr.UID] > 0 && (first == nil || tr.HeaderOffset < first.HeaderOffset) {
			first = tr
		}
	}
	return model.NewError("TRACK_DEPENDENCY_CYCLE",
		fmt.Sprintf("dependency cycle reaches TrackUID %d", first.UID),
		first.HeaderOffset)
}

// DependencyOrder returns UIDs in write order: every dependency before the
// track that joins it. Ties keep declaration order. Only valid after
// BuildGraph succeeded (acyclic graph).
func (g *Graph) DependencyOrder() []uint64 {
	done := map[uint64]bool{}
	var order []uint64
	var visit func(uid uint64)
	visit = func(uid uint64) {
		if done[uid] {
			return
		}
		done[uid] = true
		for _, dep := range g.edges[uid] {
			visit(dep)
		}
		order = append(order, uid)
	}
	for _, tr := range g.Tracks {
		visit(tr.UID)
	}
	return order
}

// ByUID looks up a track.
func (g *Graph) ByUID(uid uint64) (*model.Track, bool) {
	tr, ok := g.byUID[uid]
	return tr, ok
}
