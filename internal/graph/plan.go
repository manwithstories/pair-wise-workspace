// Package graph validates the dependency graph of a media plan and produces a
// stable topological order. Any cycle, missing track or out-of-range keyframe
// span is rejected before a single byte reaches the output.
package graph

import (
	"fmt"
	"sort"

	"avpipe/internal/media"
)

// Stage names the pipeline subsystem a node belongs to.
type Stage string

const (
	StageSource   Stage = "source"
	StageResample Stage = "resample"
	StageLayout   Stage = "layout"
	StageLoudness Stage = "loudness"
	StageSink     Stage = "sink"
	StageAudit    Stage = "audit"
)

// Span is a half-open keyframe index range [Lo, Hi).
type Span struct {
	Lo int
	Hi int
}

// Node is one unit of work in the dependency graph.
type Node struct {
	ID      string
	Stage   Stage
	Track   string
	Deps    []string
	Anchors *Span
}

// Plan is a validated graph with a deterministic execution order.
type Plan struct {
	nodes  map[string]Node
	order  []string
	tracks map[string]bool
}

// Build validates nodes against the registered timeline tracks and the total
// keyframe count, then computes a stable order: among all nodes whose
// dependencies are satisfied, the lexicographically smallest id is always
// scheduled next. Two plans built from identical input therefore always
// produce identical output.
func Build(nodes []Node, trackIDs []string, anchorCount int) (*Plan, error) {
	if len(nodes) == 0 {
		return nil, media.NewError(media.PhaseGraph, media.ExitUsage, "", 0, false, "build",
			fmt.Errorf("empty plan"))
	}
	tracks := make(map[string]bool, len(trackIDs))
	for _, id := range trackIDs {
		tracks[id] = true
	}
	byID := make(map[string]Node, len(nodes))
	for _, n := range nodes {
		if n.ID == "" {
			return nil, media.NewError(media.PhaseGraph, media.ExitUsage, n.Track, 0, false, "build",
				fmt.Errorf("node id is empty"))
		}
		if _, dup := byID[n.ID]; dup {
			return nil, media.NewError(media.PhaseGraph, media.ExitUsage, n.Track, 0, false, "build",
				fmt.Errorf("duplicate node id %q", n.ID))
		}
		if n.Stage == "" {
			return nil, media.NewError(media.PhaseGraph, media.ExitUsage, n.Track, 0, false, "build",
				fmt.Errorf("node %q has no stage", n.ID))
		}
		if n.Track != "" && !tracks[n.Track] {
			return nil, media.NewError(media.PhaseGraph, media.ExitUsage, n.Track, 0, false, "build",
				fmt.Errorf("node %q references missing track %q", n.ID, n.Track))
		}
		if n.Anchors != nil {
			if n.Anchors.Lo < 0 || n.Anchors.Hi < n.Anchors.Lo || n.Anchors.Hi > anchorCount {
				return nil, media.NewError(media.PhaseGraph, media.ExitUsage, n.Track, 0, false, "build",
					fmt.Errorf("node %q keyframe span [%d,%d) outside 0..%d",
						n.ID, n.Anchors.Lo, n.Anchors.Hi, anchorCount))
			}
		}
		for _, dep := range n.Deps {
			if dep == n.ID {
				return nil, media.NewError(media.PhaseGraph, media.ExitUsage, n.Track, 0, false, "build",
					fmt.Errorf("node %q depends on itself", n.ID))
			}
		}
		byID[n.ID] = n
	}
	// Second pass: every dependency must exist.
	for _, n := range nodes {
		for _, dep := range n.Deps {
			if _, ok := byID[dep]; !ok {
				return nil, media.NewError(media.PhaseGraph, media.ExitUsage, n.Track, 0, false, "build",
					fmt.Errorf("node %q depends on missing node %q", n.ID, dep))
			}
		}
	}

	p := &Plan{nodes: byID, tracks: tracks}
	order, err := topoOrder(byID)
	if err != nil {
		return nil, err
	}
	p.order = order
	return p, nil
}

// topoOrder is Kahn's algorithm driven by a lexicographically sorted ready
// list. The resulting order is stable for a given graph.
func topoOrder(nodes map[string]Node) ([]string, error) {
	indeg := make(map[string]int, len(nodes))
	children := make(map[string][]string, len(nodes))
	for id, n := range nodes {
		indeg[id] = len(n.Deps)
		for _, dep := range n.Deps {
			children[dep] = append(children[dep], id)
		}
	}
	for id := range children {
		sort.Strings(children[id])
	}
	var ready []string
	for id, d := range indeg {
		if d == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)

	order := make([]string, 0, len(nodes))
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		for _, child := range children[id] {
			indeg[child]--
			if indeg[child] == 0 {
				ready = append(ready, child)
				sort.Strings(ready)
			}
		}
	}
	if len(order) != len(nodes) {
		cycle := findCycle(nodes, indeg)
		return nil, media.NewError(media.PhaseGraph, media.ExitUsage, "", 0, false, "topo",
			fmt.Errorf("dependency cycle: %s", cycle))
	}
	return order, nil
}

// findCycle walks the nodes still carrying in-degree to report a concrete
// cycle path, which makes misconfiguration diagnosable.
func findCycle(nodes map[string]Node, indeg map[string]int) string {
	remaining := make(map[string]bool)
	var start string
	for id, d := range indeg {
		if d > 0 {
			remaining[id] = true
			if start == "" || id < start {
				start = id
			}
		}
	}
	if start == "" {
		return "<unknown>"
	}
	path := []string{start}
	seen := map[string]int{start: 0}
	cur := start
	for {
		next := ""
		for _, dep := range nodes[cur].Deps {
			if remaining[dep] {
				next = dep
				break
			}
		}
		if next == "" {
			return fmt.Sprint(path)
		}
		if idx, ok := seen[next]; ok {
			path = append(path, next)
			return fmt.Sprint(path[idx:])
		}
		seen[next] = len(path)
		path = append(path, next)
		cur = next
	}
}

// Order returns the stable execution order.
func (p *Plan) Order() []string { return append([]string(nil), p.order...) }

// Node returns a node by id.
func (p *Plan) Node(id string) (Node, bool) {
	n, ok := p.nodes[id]
	return n, ok
}

// Len returns the node count.
func (p *Plan) Len() int { return len(p.nodes) }
