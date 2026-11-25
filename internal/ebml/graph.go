package ebml

import (
	"fmt"
	"sort"

	"avcheck/internal/media"
)

// Graph is the track dependency DAG. An edge a->b means b must be finalized
// before a, because a was authored against b (cues against video, video
// against audio clock, etc.). Unknown tracks and cycles are hard errors: the
// whole point is that dependencies are never guessed.
type Graph struct {
	nodes map[uint64]*Track
	deps  map[uint64][]uint64 // track -> tracks it depends on
	fault *media.Faults
}

// NewGraph builds and validates the graph from the document's track list.
func NewGraph(doc *Document) (*Graph, error) {
	g := &Graph{
		nodes: make(map[uint64]*Track, len(doc.Tracks)),
		deps:  make(map[uint64][]uint64, len(doc.Tracks)),
		fault: doc.Faults,
	}
	for i := range doc.Tracks {
		t := &doc.Tracks[i]
		if t.Number == 0 {
			g.fault.Addf(media.LevelAbort, media.CodeBadTrack, 0, 0, "track with number 0")
			return g, g.fault
		}
		if _, dup := g.nodes[t.Number]; dup {
			g.fault.Addf(media.LevelAbort, media.CodeBadTrack, t.Number, 0, "duplicate track number")
			return g, g.fault
		}
		g.nodes[t.Number] = t
	}
	for i := range doc.Tracks {
		t := &doc.Tracks[i]
		for _, d := range t.DependsOn {
			if d == t.Number {
				g.fault.Addf(media.LevelAbort, media.CodeDependencyCycle, t.Number, 0, "track depends on itself")
				return g, g.fault
			}
			if _, ok := g.nodes[d]; !ok {
				g.fault.Addf(media.LevelAbort, media.CodeUnknownTrack, t.Number, 0, "depends on undeclared track %d", d)
				return g, g.fault
			}
			g.deps[t.Number] = append(g.deps[t.Number], d)
		}
		sort.Slice(g.deps[t.Number], func(i, j int) bool {
			return g.deps[t.Number][i] < g.deps[t.Number][j]
		})
	}
	if len(doc.Tracks) > 0 {
		if _, err := g.FinishOrder(); err != nil {
			return g, err
		}
	}
	return g, nil
}

// Dependencies returns the sorted direct dependencies of a track.
func (g *Graph) Dependencies(n uint64) []uint64 { return g.deps[n] }

// FinishOrder returns track numbers such that every dependency appears
// before its dependents (a topological order), i.e. the order in which
// tracks may be flushed at end of stream. It is deterministic: ready nodes
// are emitted in ascending track-number order.
func (g *Graph) FinishOrder() ([]uint64, error) {
	indeg := make(map[uint64]int, len(g.nodes))
	dependents := make(map[uint64][]uint64, len(g.nodes))
	for n := range g.nodes {
		indeg[n] = len(g.deps[n])
		for _, d := range g.deps[n] {
			dependents[d] = append(dependents[d], n)
		}
	}
	ready := make([]uint64, 0, len(g.nodes))
	for n, k := range indeg {
		if k == 0 {
			ready = append(ready, n)
		}
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i] < ready[j] })
	order := make([]uint64, 0, len(g.nodes))
	for len(ready) > 0 {
		n := ready[0]
		ready = ready[1:]
		order = append(order, n)
		next := dependents[n]
		sort.Slice(next, func(i, j int) bool { return next[i] < next[j] })
		for _, m := range next {
			indeg[m]--
			if indeg[m] == 0 {
				ready = append(ready, m)
			}
		}
		sort.Slice(ready, func(i, j int) bool { return ready[i] < ready[j] })
	}
	if len(order) != len(g.nodes) {
		var stuck []uint64
		for n, k := range indeg {
			if k > 0 {
				stuck = append(stuck, n)
			}
		}
		sort.Slice(stuck, func(i, j int) bool { return stuck[i] < stuck[j] })
		g.fault.Addf(media.LevelAbort, media.CodeDependencyCycle, stuck[0], 0,
			"cycle among tracks %v", stuck)
		return nil, g.fault
	}
	return order, nil
}

// Anchor returns the audio track chosen as the sync reference. Exactly one
// audio track is required; preferring the lowest track number keeps runs
// reproducible.
func (g *Graph) Anchor() (uint64, error) {
	var audio []uint64
	for n, t := range g.nodes {
		if t.Kind == media.KindAudio {
			audio = append(audio, n)
		}
	}
	if len(audio) == 0 {
		g.fault.Addf(media.LevelAbort, media.CodeUnknownTrack, 0, 0, "no audio anchor track declared")
		return 0, g.fault
	}
	sort.Slice(audio, func(i, j int) bool { return audio[i] < audio[j] })
	return audio[0], nil
}

// Tracks returns declared track numbers sorted ascending.
func (g *Graph) Tracks() []uint64 {
	out := make([]uint64, 0, len(g.nodes))
	for n := range g.nodes {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Kind reports the kind of a track, or KindUnknown.
func (g *Graph) Kind(n uint64) media.TrackKind {
	if t, ok := g.nodes[n]; ok {
		return t.Kind
	}
	return media.KindUnknown
}

// String renders the graph as a stable diagnostic listing.
func (g *Graph) String() string {
	s := ""
	for _, n := range g.Tracks() {
		s += fmt.Sprintf("%d(%s)->%v ", n, g.Kind(n), g.deps[n])
	}
	return s
}
