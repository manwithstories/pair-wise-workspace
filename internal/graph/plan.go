// Package graph builds and orders the dependency graph for tracks and stages.
package graph

import (
	"fmt"
	"sort"
	"strings"

	"fieldcut/internal/clock"
)

// Node is one track, processing stage, or output artifact.
type Node struct {
	ID        string            `json:"id"`
	TrackID   int               `json:"track_id"`
	Kind      string            `json:"kind"`
	DependsOn []string          `json:"depends_on,omitempty"`
	InputHash string            `json:"input_hash,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// Plan is a validated track dependency graph.
type Plan struct {
	Nodes map[string]Node `json:"nodes"`
}

// NewPlan constructs a graph and rejects duplicate node IDs.
func NewPlan(nodes ...Node) (*Plan, error) {
	p := &Plan{Nodes: make(map[string]Node, len(nodes))}
	for _, n := range nodes {
		if n.ID == "" {
			return nil, clock.Errorf(clock.CodeInvalidArgument, n.TrackID, "graph node has empty ID")
		}
		if _, exists := p.Nodes[n.ID]; exists {
			return nil, clock.Errorf(clock.CodeInvalidArgument, n.TrackID, "duplicate graph node %q", n.ID)
		}
		p.Nodes[n.ID] = n
	}
	return p, nil
}

// Add inserts one node.
func (p *Plan) Add(n Node) error {
	if p.Nodes == nil {
		p.Nodes = map[string]Node{}
	}
	if _, exists := p.Nodes[n.ID]; exists {
		return clock.Errorf(clock.CodeInvalidArgument, n.TrackID, "duplicate graph node %q", n.ID)
	}
	p.Nodes[n.ID] = n
	return nil
}

// TopoSort returns a stable topological order. Ready IDs are sorted, so the
// same graph topology always yields the same execution order.
func TopoSort(nodes map[string]Node) ([]string, error) {
	deps := make(map[string]map[string]bool, len(nodes))
	indegree := make(map[string]int, len(nodes))
	for id, n := range nodes {
		indegree[id] = 0
		deps[id] = map[string]bool{}
		for _, dep := range n.DependsOn {
			if _, ok := nodes[dep]; !ok {
				return nil, clock.Errorf(clock.CodeInvalidArgument, n.TrackID,
					"node %q depends on missing node %q", id, dep)
			}
			if !deps[id][dep] {
				deps[id][dep] = true
				indegree[id]++
			}
		}
	}

	ready := make([]string, 0)
	for id, d := range indegree {
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

		next := make([]string, 0)
		for candidate, n := range nodes {
			if indegree[candidate] == 0 || candidate == id {
				continue
			}
			for _, dep := range n.DependsOn {
				if dep == id {
					indegree[candidate]--
					if indegree[candidate] == 0 {
						next = append(next, candidate)
					}
				}
			}
		}
		ready = append(ready, next...)
		sort.Strings(ready)
	}

	if len(order) != len(nodes) {
		remaining := make([]string, 0)
		for id, d := range indegree {
			if d > 0 {
				remaining = append(remaining, id)
			}
		}
		sort.Strings(remaining)
		return nil, &clock.Error{
			Code: clock.CodeCycle, TrackID: -1,
			OriginalValue: strings.Join(remaining, ","),
			Message:       fmt.Sprintf("dependency cycle involves: %s", strings.Join(remaining, ", ")),
		}
	}
	return order, nil
}

// TopoSort is a method wrapper.
func (p *Plan) TopoSort() ([]string, error) { return TopoSort(p.Nodes) }

// ColorDescription preserves one track's color metadata.
type ColorDescription struct {
	TrackID     int    `json:"track_id"`
	Primaries   string `json:"primaries,omitempty"`
	Transfer    string `json:"transfer,omitempty"`
	Matrix      string `json:"matrix,omitempty"`
	FullRange   bool   `json:"full_range"`
	HasMetadata bool   `json:"has_metadata"`
}

// ColorPolicy states exactly how color conflicts are resolved.
type ColorPolicy struct {
	Mode             string `json:"mode"` // strict | prefer_track
	PreferredTrackID int    `json:"preferred_track_id,omitempty"`
}

// ColorDecision records a selected or rejected conflict. It prevents silent
// metadata overwrite.
type ColorDecision struct {
	Selected  ColorDescription   `json:"selected"`
	Conflicts []ColorDescription `json:"conflicts,omitempty"`
	Reason    string             `json:"reason"`
}

func colorEqual(a, b ColorDescription) bool {
	return a.Primaries == b.Primaries && a.Transfer == b.Transfer &&
		a.Matrix == b.Matrix && a.FullRange == b.FullRange
}

// ResolveColor applies an explicit policy to per-track color descriptions.
func ResolveColor(descs []ColorDescription, policy ColorPolicy) (ColorDecision, error) {
	present := make([]ColorDescription, 0, len(descs))
	for _, d := range descs {
		if d.HasMetadata {
			present = append(present, d)
		}
	}
	if len(present) == 0 {
		return ColorDecision{Reason: "no color descriptors present"}, nil
	}
	selected := present[0]
	conflicts := make([]ColorDescription, 0)
	for _, d := range present[1:] {
		if !colorEqual(selected, d) {
			conflicts = append(conflicts, d)
		}
	}
	if len(conflicts) == 0 {
		return ColorDecision{Selected: selected, Reason: "all color descriptors agree"}, nil
	}

	switch policy.Mode {
	case "strict":
		return ColorDecision{}, &clock.Error{
			Code: clock.CodeColorConflict, TrackID: selected.TrackID,
			OriginalValue: fmt.Sprintf("selected_track=%d conflicts=%d", selected.TrackID, len(conflicts)+1),
			Message:       "conflicting color metadata and strict policy refuses to choose",
		}
	case "prefer_track":
		for _, d := range present {
			if d.TrackID == policy.PreferredTrackID {
				allConflicts := make([]ColorDescription, 0, len(present)-1)
				for _, x := range present {
					if x.TrackID != d.TrackID && !colorEqual(d, x) {
						allConflicts = append(allConflicts, x)
					}
				}
				return ColorDecision{Selected: d, Conflicts: allConflicts,
					Reason: fmt.Sprintf("explicit prefer_track policy selected track %d", d.TrackID)}, nil
			}
		}
		return ColorDecision{}, clock.Errorf(clock.CodeConfig, -1,
			"color policy prefers unknown track %d", policy.PreferredTrackID)
	default:
		return ColorDecision{}, clock.Errorf(clock.CodeConfig, -1, "unknown color policy %q", policy.Mode)
	}
}
