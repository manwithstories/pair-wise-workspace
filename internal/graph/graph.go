package graph

import (
	"errors"
	"fmt"
	"sort"

	"mediasim/internal/clock"
)

// NodeID identifies one unit of work in the plan graph. It is stable across
// runs so reports and golden outputs can key on it.
type NodeID string

// NodeKind distinguishes the work a node performs.
type NodeKind string

const (
	NodeSegment    NodeKind = "segment"   // decode/parse one segment's source
	NodeEditPiece  NodeKind = "editpiece" // one side of an edit point inside a segment
	NodeTransition NodeKind = "transition"
)

// Node is one scheduled unit. Deps lists the nodes that must finish first.
type Node struct {
	ID       NodeID
	Kind     NodeKind
	Track    string
	Segment  string
	Start    int64
	Duration int64
	Deps     []NodeID
}

// Interval is the node's half-open tick range on the timeline.
func (n Node) Interval() clock.Interval {
	return clock.Interval{Start: n.Start, End: n.Start + n.Duration}
}

// Span locates the node for error reporting.
func (n Node) Span() clock.Span {
	return clock.NewSpan(n.Track, n.Segment, n.Interval())
}

// Graph is the validated DAG derived from a Timeline.
type Graph struct {
	Timeline *Timeline
	nodes    []Node
	byID     map[NodeID]int
}

// Nodes returns the nodes in stable insertion order.
func (g *Graph) Nodes() []Node { return g.nodes }

// Node looks a node up by ID.
func (g *Graph) Node(id NodeID) (Node, bool) {
	i, ok := g.byID[id]
	if !ok {
		return Node{}, false
	}
	return g.nodes[i], true
}

// Build derives the DAG from a validated timeline. Dependencies combine:
//   - explicit segment Requires (cross-track ordering),
//   - the previous segment on the same track,
//   - an edit-piece node depends on its parent segment,
//   - a transition depends on both of its endpoints.
//
// Missing dependencies and cycles are classified and reported with the
// track and tick position of the node that closes the cycle.
func Build(t *Timeline) (*Graph, error) {
	g := &Graph{Timeline: t, byID: make(map[NodeID]int)}
	if err := g.addSegments(); err != nil {
		return nil, err
	}
	if err := g.addEditPieces(); err != nil {
		return nil, err
	}
	if err := g.addTransitions(); err != nil {
		return nil, err
	}
	if _, err := g.topoOrder(); err != nil {
		return nil, err
	}
	return g, nil
}

func segmentNodeID(segID string) NodeID { return NodeID("seg:" + segID) }

func (g *Graph) addSegments() error {
	t := g.Timeline
	segSet := make(map[string]Segment)
	for _, tr := range t.Tracks {
		for _, s := range tr.Segments {
			segSet[s.ID] = s
		}
	}
	var errs []error
	for _, tr := range t.Tracks {
		ordered := append([]Segment(nil), tr.Segments...)
		sort.SliceStable(ordered, func(i, j int) bool {
			if ordered[i].Start != ordered[j].Start {
				return ordered[i].Start < ordered[j].Start
			}
			return ordered[i].ID < ordered[j].ID
		})
		var prev NodeID
		for _, s := range ordered {
			n := Node{
				ID: segmentNodeID(s.ID), Kind: NodeSegment, Track: tr.ID, Segment: s.ID,
				Start: s.Start, Duration: s.Duration,
			}
			if prev != "" {
				n.Deps = append(n.Deps, prev)
			}
			for _, req := range s.Requires {
				if _, ok := segSet[req]; !ok {
					errs = append(errs, invalid(clock.CatGraph, n.Span(),
						"segment %s requires missing segment %q", s.ID, req))
					continue
				}
				n.Deps = append(n.Deps, segmentNodeID(req))
			}
			if err := g.addNode(n); err != nil {
				errs = append(errs, err)
			}
			prev = n.ID
		}
	}
	return joinOrNil(errs)
}

func (g *Graph) addEditPieces() error {
	var errs []error
	for _, tr := range g.Timeline.Tracks {
		for _, s := range tr.Segments {
			bounds := append([]int64{0}, s.EditPoints...)
			bounds = append(bounds, s.Duration)
			for i := 0; i+1 < len(bounds); i++ {
				start, dur := bounds[i], bounds[i+1]-bounds[i]
				if dur == 0 {
					continue
				}
				n := Node{
					ID:   NodeID(fmt.Sprintf("piece:%s:%d", s.ID, i)),
					Kind: NodeEditPiece, Track: tr.ID, Segment: s.ID,
					Start: s.Start + start, Duration: dur,
					Deps: []NodeID{segmentNodeID(s.ID)},
				}
				if err := g.addNode(n); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	return joinOrNil(errs)
}

func (g *Graph) addTransitions() error {
	var errs []error
	for _, tr := range g.Timeline.Transitions {
		from, _ := g.Timeline.Segment(tr.From)
		n := Node{
			ID:       NodeID("tr:" + tr.ID),
			Kind:     NodeTransition,
			Track:    from.Track,
			Segment:  tr.From,
			Start:    from.Start + from.Duration - tr.Duration,
			Duration: tr.Duration,
			Deps:     []NodeID{segmentNodeID(tr.From), segmentNodeID(tr.To)},
		}
		if err := g.addNode(n); err != nil {
			errs = append(errs, err)
		}
	}
	return joinOrNil(errs)
}

// Deps returns the node IDs a node depends on, for tests and diagnostics.
func (g *Graph) Deps(id NodeID) []NodeID {
	n, ok := g.Node(id)
	if !ok {
		return nil
	}
	return append([]NodeID(nil), n.Deps...)
}

func (g *Graph) addNode(n Node) error {
	if _, dup := g.byID[n.ID]; dup {
		return invalid(clock.CatGraph, n.Span(), "duplicate node id %s", n.ID)
	}
	g.byID[n.ID] = len(g.nodes)
	g.nodes = append(g.nodes, n)
	return nil
}

func joinOrNil(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}
