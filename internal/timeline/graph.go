// Package timeline assembles recovered carriers into a dependency-ordered
// track graph and one shared rational time base. Every presentation
// timestamp is an exact fraction of a second; cross-sample-rate mapping
// uses integer sample conversion so PTS can never accumulate float drift.
package timeline

import (
	"fmt"
	"sort"

	"audiorestore/internal/model"
)

// Anchor is a video reference point the audio timeline must align to.
type Anchor struct {
	ID        string
	AtPTS     model.Rational // seconds on the reference clock
	RefRate   model.SampleRate
	RefSample int64 // equivalent sample index at RefRate
}

// Track is one recovered audio carrier placed on the shared timeline.
type Track struct {
	ID         string
	Rate       model.SampleRate
	Channels   int
	BitDepth   uint16
	Layout     string
	Color      string
	Frames     int64
	StartFrame int64 // offset on the track's own sample clock
	DependsOn  []string
	// Degraded records layout/color problems without rewriting source
	// channel ordering.
	Degraded []string
}

// Edge is one dependency relation in the track graph.
type Edge struct{ From, To string }

// Graph is the validated, topologically ordered track timeline.
type Graph struct {
	Tracks  []*Track
	order   []string
	edges   []Edge
	anchors []Anchor
	refRate model.SampleRate
}

// NewGraph builds the graph, validates dependency references and returns a
// deterministic topological ordering (ties broken by track id).
func NewGraph(tracks []*Track, anchors []Anchor, refRate model.SampleRate) (*Graph, error) {
	g := &Graph{anchors: anchors, refRate: refRate}
	byID := make(map[string]*Track, len(tracks))
	for _, t := range tracks {
		if _, dup := byID[t.ID]; dup {
			return nil, model.TrackedError{Kind: model.ErrInvalidParam, Track: t.ID,
				Msg: "duplicate track id"}
		}
		byID[t.ID] = t
		g.Tracks = append(g.Tracks, t)
	}
	indeg := make(map[string]int, len(tracks))
	adj := make(map[string][]string, len(tracks))
	for _, t := range tracks {
		indeg[t.ID] = 0
	}
	for _, t := range tracks {
		for _, dep := range t.DependsOn {
			if _, ok := byID[dep]; !ok {
				return nil, model.TrackedError{Kind: model.ErrInvalidParam, Track: t.ID,
					Msg: fmt.Sprintf("depends on unknown track %q", dep)}
			}
			adj[dep] = append(adj[dep], t.ID)
			indeg[t.ID]++
			g.edges = append(g.edges, Edge{From: dep, To: t.ID})
		}
	}
	// Kahn's algorithm with lexicographic tie-breaking for determinism.
	queue := make([]string, 0, len(tracks))
	for id, d := range indeg {
		if d == 0 {
			queue = append(queue, id)
		}
	}
	for len(queue) > 0 {
		sort.Strings(queue)
		id := queue[0]
		queue = queue[1:]
		g.order = append(g.order, id)
		next := append([]string(nil), adj[id]...)
		sort.Strings(next)
		for _, to := range next {
			indeg[to]--
			if indeg[to] == 0 {
				queue = append(queue, to)
			}
		}
	}
	if len(g.order) != len(tracks) {
		return nil, model.TrackedError{Kind: model.ErrInvalidParam, Msg: "dependency cycle"}
	}
	return g, nil
}

// Order returns track ids in dependency-sorted order.
func (g *Graph) Order() []string { return append([]string(nil), g.order...) }

// Edges returns the dependency edges.
func (g *Graph) Edges() []Edge { return append([]Edge(nil), g.edges...) }

// ByID looks up a track.
func (g *Graph) ByID(id string) *Track {
	for _, t := range g.Tracks {
		if t.ID == id {
			return t
		}
	}
	return nil
}
