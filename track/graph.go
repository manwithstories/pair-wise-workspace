// Package track models the dependency graph of media tracks.
//
// Subtitle and timecode inputs flow into a unified timeline; RTP media tracks
// may reference those timeline tracks. The graph decides scheduling order:
// a track is only scheduled once every track it depends on has completed.
// Cycles and references to unknown tracks are rejected before any work starts,
// with the offending track and edge located in the diagnostic.
package track

import (
	"sort"

	"mediad/diag"
)

// Kind classifies a track vertex.
type Kind string

const (
	KindRTP      Kind = "rtp"
	KindSRT      Kind = "srt"
	KindVTT      Kind = "vtt"
	KindTimecode Kind = "timecode"
)

// Track is one vertex of the dependency graph.
type Track struct {
	ID   string
	Kind Kind
	// Deps lists track IDs that must be processed before this one.
	Deps []string
}

// Edge is a directed dependency: From must complete before To.
type Edge struct {
	From string
	To   string
}

// Graph is an immutable-on-build set of tracks and dependency edges.
type Graph struct {
	tracks map[string]*Track
	order  []string // insertion order for stable diagnostics
	edges  []Edge
}

// NewGraph validates and builds a graph. Duplicate IDs and dependencies on
// unknown tracks are rejected; cycle detection runs in Topo so both checks can
// be exercised independently in tests.
func NewGraph(tracks []Track) (*Graph, error) {
	g := &Graph{tracks: map[string]*Track{}}
	if len(tracks) > 0 {
		lim := diag.DefaultLimits()
		if len(tracks) > lim.MaxTracks {
			return nil, diag.New(diag.EParam, "", "track count %d exceeds limit %d", len(tracks), lim.MaxTracks)
		}
	}
	for i := range tracks {
		t := tracks[i]
		if t.ID == "" {
			return nil, diag.New(diag.EParam, "", "track #%d has empty ID", i)
		}
		if _, exists := g.tracks[t.ID]; exists {
			return nil, diag.New(diag.EParam, t.ID, "duplicate track ID")
		}
		cp := t
		g.tracks[t.ID] = &cp
		g.order = append(g.order, t.ID)
	}
	for _, t := range tracks {
		for _, dep := range t.Deps {
			if _, ok := g.tracks[dep]; !ok {
				return nil, diag.New(diag.EGraphMissing, t.ID,
					"dependency edge %s -> %s targets unknown track %q", dep, t.ID, dep)
			}
			g.edges = append(g.edges, Edge{From: dep, To: t.ID})
		}
	}
	return g, nil
}

// Get returns a track by ID.
func (g *Graph) Get(id string) (*Track, bool) {
	t, ok := g.tracks[id]
	return t, ok
}

// IDs lists tracks in insertion order.
func (g *Graph) IDs() []string { return append([]string(nil), g.order...) }

// Edges lists all dependency edges in declaration order.
func (g *Graph) Edges() []Edge { return append([]Edge(nil), g.edges...) }

// Cycle describes a detected dependency cycle with the exact closing edge.
type Cycle struct {
	// Path is the sequence of track IDs forming the cycle, first ID repeated
	// at the end, e.g. ["a","b","c","a"].
	Path []string
	// ClosingEdge is the edge that completes the cycle.
	ClosingEdge Edge
}

// Topo returns tracks in dependency-respecting order. Ties break
// alphabetically so scheduling is deterministic across runs. A cycle is
// reported with the full path and the edge that closes it.
func (g *Graph) Topo() ([]string, *Cycle, error) {
	indeg := make(map[string]int, len(g.order))
	adj := make(map[string][]string, len(g.order))
	for _, id := range g.order {
		indeg[id] = 0
	}
	for _, e := range g.edges {
		adj[e.From] = append(adj[e.From], e.To)
		indeg[e.To]++
	}
	// Kahn's algorithm with a sorted ready set for stable output.
	ready := make([]string, 0)
	for _, id := range g.order {
		if indeg[id] == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	out := make([]string, 0, len(g.order))
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		out = append(out, id)
		next := append([]string(nil), adj[id]...)
		sort.Strings(next)
		for _, to := range next {
			indeg[to]--
			if indeg[to] == 0 {
				ready = append(ready, to)
				sort.Strings(ready)
			}
		}
	}
	if len(out) == len(g.order) {
		return out, nil, nil
	}
	cyc := g.findCycle()
	return nil, &cyc, diag.New(diag.EGraphCycle, cyc.ClosingEdge.To,
		"dependency cycle %v closes on edge %s -> %s",
		cyc.Path, cyc.ClosingEdge.From, cyc.ClosingEdge.To)
}

// findCycle runs DFS along real dependency edges (From -> To). Vertices left
// after Kahn pruning are exactly the cycle participants; the first DFS back
// edge is the edge that closes the cycle, with the stack giving its path.
func (g *Graph) findCycle() Cycle {
	adj := make(map[string][]string, len(g.order))
	for _, e := range g.edges {
		adj[e.From] = append(adj[e.From], e.To)
	}
	color := map[string]int{} // 0 white, 1 gray, 2 black
	stack := []string{}
	var cyc Cycle
	var dfs func(id string) bool
	dfs = func(id string) bool {
		color[id] = 1
		stack = append(stack, id)
		next := append([]string(nil), adj[id]...)
		sort.Strings(next)
		for _, to := range next {
			switch color[to] {
			case 0:
				if dfs(to) {
					return true
				}
			case 1:
				// Back edge id -> to closes a cycle along the current stack.
				start := indexOf(stack, to)
				path := append([]string(nil), stack[start:]...)
				path = append(path, to)
				cyc = Cycle{Path: path, ClosingEdge: Edge{From: id, To: to}}
				return true
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = 2
		return false
	}
	ids := append([]string(nil), g.order...)
	sort.Strings(ids)
	for _, id := range ids {
		if color[id] == 0 && dfs(id) {
			return cyc
		}
	}
	return cyc
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}
