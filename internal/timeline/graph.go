package timeline

import (
	"sort"
	"strings"

	"mediamix/internal/core"
)

// NodeKind describes what a track contributes to the mix.
type NodeKind string

const (
	KindVideo NodeKind = "video"
	KindAudio NodeKind = "audio"
)

// Node is one explicit track in the processing graph. Dependencies are other
// track ids that must be processed first (for example audio beds feeding a mix
// bus).
//
// ExclusiveGroup names a set of tracks that contend for one non-shareable
// resource, such as successive takes assigned the same recording slot; members
// of a group may not declare overlapping spans. Tracks without a group are
// unrestricted: two camera angles covering the same wall-clock interval are
// normal and must not be reported as an overlap fault.
type Node struct {
	Track          string   `json:"track"`
	Kind           NodeKind `json:"kind"`
	DependsOn      []string `json:"depends_on,omitempty"`
	ExclusiveGroup string   `json:"exclusive_group,omitempty"`
	Span           Span     `json:"span"`
	Segments       []Span   `json:"segments,omitempty"`
}

// Graph is an explicit, ordered track graph. It never infers dependencies.
type Graph struct {
	nodes map[string]*Node
	ids   []string
	max   int
}

// DefaultMaxTracks bounds a single run so a malformed graph cannot make the
// pipeline allocate unbounded state.
const DefaultMaxTracks = 64

// NewGraph creates an empty graph with a hard track limit.
func NewGraph(maxTracks int) (*Graph, error) {
	if maxTracks <= 0 {
		maxTracks = DefaultMaxTracks
	}
	if maxTracks > 4096 {
		return nil, &core.Error{Code: core.CodeOverLimit, Detail: "track limit above 4096", Fatal: true}
	}
	return &Graph{nodes: make(map[string]*Node), max: maxTracks}, nil
}

// Add registers a track. Duplicate ids and self dependencies are rejected.
func (g *Graph) Add(n Node) error {
	if n.Track == "" {
		return &core.Error{Code: core.CodeBadGraph, Detail: "track id is empty", Fatal: true}
	}
	if n.Kind != KindVideo && n.Kind != KindAudio {
		return &core.Error{Code: core.CodeBadGraph, Track: n.Track, Detail: "unknown track kind", Fatal: true}
	}
	if _, exists := g.nodes[n.Track]; exists {
		return &core.Error{Code: core.CodeBadGraph, Track: n.Track, Detail: "duplicate track id", Fatal: true}
	}
	if len(g.nodes) >= g.max {
		return &core.Error{Code: core.CodeOverLimit, Track: n.Track, Detail: "track limit reached", Fatal: true}
	}
	if !n.Span.Valid() {
		return &core.Error{Code: core.CodeBadGraph, Track: n.Track, Detail: "invalid declared span", Fatal: true}
	}
	for _, dep := range n.DependsOn {
		if dep == n.Track {
			return &core.Error{Code: core.CodeCycle, Track: n.Track, Detail: "track depends on itself", Fatal: true}
		}
	}
	segs := make([]Span, len(n.Segments))
	copy(segs, n.Segments)
	for i := 1; i < len(segs); i++ {
		if !segs[i].Valid() || segs[i].Start < segs[i-1].End {
			return &core.Error{Code: core.CodeOverlap, Track: n.Track, Detail: "segments overlap or are unordered", Fatal: true}
		}
	}
	cp := n
	cp.DependsOn = append([]string(nil), n.DependsOn...)
	cp.Segments = segs
	g.nodes[n.Track] = &cp
	g.ids = append(g.ids, n.Track)
	return nil
}

// Node returns a registered node.
func (g *Graph) Node(track string) (*Node, bool) {
	n, ok := g.nodes[track]
	return n, ok
}

// Len returns the number of registered tracks.
func (g *Graph) Len() int { return len(g.nodes) }

// Tracks returns registered ids in insertion order.
func (g *Graph) Tracks() []string { return append([]string(nil), g.ids...) }

// Validate checks missing dependencies, cycles and same-group overlap. It
// returns every problem found, never just the first, so reports stay useful.
func (g *Graph) Validate() []*core.Error {
	var errs []*core.Error
	for _, id := range g.ids {
		n := g.nodes[id]
		for _, dep := range n.DependsOn {
			if _, ok := g.nodes[dep]; !ok {
				e := &core.Error{Code: core.CodeMissingDep, Track: id,
					Detail: "missing dependency " + dep, Fatal: true}
				errs = append(errs, e)
			}
		}
	}
	if cycle := g.findCycle(); cycle != nil {
		errs = append(errs, &core.Error{
			Code:   core.CodeCycle,
			Track:  cycle[0],
			Detail: "dependency cycle: " + strings.Join(cycle, " -> "),
			Fatal:  true,
		})
	}
	// Exclusive groups are mutually exclusive by definition: members contend
	// for one non-shareable slot.
	byGroup := make(map[string][]*Node)
	for _, id := range g.ids {
		n := g.nodes[id]
		if n.ExclusiveGroup == "" {
			continue
		}
		byGroup[n.ExclusiveGroup] = append(byGroup[n.ExclusiveGroup], n)
	}
	for group, nodes := range byGroup {
		sort.Slice(nodes, func(i, j int) bool { return nodes[i].Track < nodes[j].Track })
		for i := 1; i < len(nodes); i++ {
			a, b := nodes[i-1], nodes[i]
			if a.Span.Overlaps(b.Span) {
				errs = append(errs, &core.Error{
					Code:   core.CodeOverlap,
					Track:  b.Track,
					Detail: "declared interval overlaps " + a.Track + " in group " + group,
					Fatal:  true,
				})
			}
		}
	}
	sort.SliceStable(errs, func(i, j int) bool {
		if errs[i].Track != errs[j].Track {
			return errs[i].Track < errs[j].Track
		}
		return errs[i].Code < errs[j].Code
	})
	return errs
}

// findCycle returns the smallest cycle path found by DFS, or nil.
func (g *Graph) findCycle() []string {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(g.nodes))
	stack := make([]string, 0, len(g.nodes))
	index := make(map[string]int, len(g.nodes))
	var cycle []string
	var visit func(id string) bool
	visit = func(id string) bool {
		color[id] = gray
		index[id] = len(stack)
		stack = append(stack, id)
		deps := append([]string(nil), g.nodes[id].DependsOn...)
		sort.Strings(deps)
		for _, dep := range deps {
			if _, ok := g.nodes[dep]; !ok {
				continue
			}
			switch color[dep] {
			case white:
				if visit(dep) {
					return true
				}
			case gray:
				start := index[dep]
				cycle = append([]string(nil), stack[start:]...)
				cycle = append(cycle, dep)
				return true
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = black
		return false
	}
	ids := append([]string(nil), g.ids...)
	sort.Strings(ids)
	for _, id := range ids {
		if color[id] == white && visit(id) {
			return cycle
		}
	}
	return nil
}

// Order returns a deterministic topological order. Ties between ready nodes
// are broken lexicographically so identical inputs always produce identical
// reports. A cycle or missing dependency is returned as an error.
func (g *Graph) Order() ([]string, error) {
	errs := g.Validate()
	if len(errs) > 0 {
		return nil, errs[0]
	}
	indeg := make(map[string]int, len(g.nodes))
	children := make(map[string][]string, len(g.nodes))
	for _, id := range g.ids {
		n := g.nodes[id]
		indeg[id] = len(n.DependsOn)
		for _, dep := range n.DependsOn {
			children[dep] = append(children[dep], id)
		}
	}
	ready := make([]string, 0, len(g.nodes))
	for id, d := range indeg {
		if d == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	order := make([]string, 0, len(g.nodes))
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		kids := append([]string(nil), children[id]...)
		sort.Strings(kids)
		for _, kid := range kids {
			indeg[kid]--
			if indeg[kid] == 0 {
				ready = append(ready, kid)
			}
		}
		sort.Strings(ready)
	}
	if len(order) != len(g.nodes) {
		return nil, &core.Error{Code: core.CodeCycle, Detail: "graph is not acyclic", Fatal: true}
	}
	return order, nil
}

// Dependencies returns a node's declared dependencies sorted.
func (g *Graph) Dependencies(track string) []string {
	n, ok := g.nodes[track]
	if !ok {
		return nil
	}
	deps := append([]string(nil), n.DependsOn...)
	sort.Strings(deps)
	return deps
}

// CheckSegments verifies that per-track segment intervals do not overlap.
func CheckSegments(track string, spans []Span) []*core.Error {
	var errs []*core.Error
	for i := 1; i < len(spans); i++ {
		if !spans[i].Valid() {
			errs = append(errs, &core.Error{Code: core.CodeBadTimestamp, Track: track,
				Detail: "invalid segment interval", Fatal: true})
			continue
		}
		if spans[i].Overlaps(spans[i-1]) {
			errs = append(errs, &core.Error{Code: core.CodeOverlap, Track: track,
				Detail: "segments overlap", Fatal: true})
		}
	}
	return errs
}

// MergeSpans merges overlapping or touch-adjacent spans, sorted by start.
func MergeSpans(spans []Span) []Span {
	if len(spans) == 0 {
		return nil
	}
	cp := append([]Span(nil), spans...)
	sort.Slice(cp, func(i, j int) bool { return cp[i].Start < cp[j].Start })
	out := []Span{cp[0]}
	for _, s := range cp[1:] {
		last := &out[len(out)-1]
		if s.Start <= last.End {
			if s.End > last.End {
				last.End = s.End
			}
			continue
		}
		out = append(out, s)
	}
	return out
}
