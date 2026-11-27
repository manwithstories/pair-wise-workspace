// Package graph models track dependencies and produces a deterministic
// repair schedule. Audio tracks depend on a video reference so that clock
// alignment happens before indexing; every index node depends on all
// tracks it references.
package graph

import (
	"fmt"
	"sort"
)

// Kind classifies a plan node.
type Kind string

const (
	KindVideo Kind = "video"
	KindAudio Kind = "audio"
	KindIndex Kind = "index"
)

// Node is one schedulable unit.
type Node struct {
	ID   string
	Kind Kind
	Deps []string
}

// Plan is a validated dependency graph.
type Plan struct {
	Nodes  map[string]Node
	Order  []string
	Layers [][]string
}

// Build creates a plan from tracks in declaration order. The first video
// track becomes the sync reference for every audio track. The index node
// depends on every track.
func Build(videos, audios []string) (*Plan, error) {
	p := &Plan{Nodes: map[string]Node{}}
	seen := map[string]bool{}
	var order []string
	add := func(id string, kind Kind, deps []string) error {
		if id == "" {
			return fmt.Errorf("graph: empty node id")
		}
		if seen[id] {
			return fmt.Errorf("graph: duplicate node %q", id)
		}
		seen[id] = true
		p.Nodes[id] = Node{ID: id, Kind: kind, Deps: append([]string(nil), deps...)}
		order = append(order, id)
		return nil
	}
	for _, v := range videos {
		if err := add(v, KindVideo, nil); err != nil {
			return nil, err
		}
	}
	ref := ""
	if len(videos) > 0 {
		ref = videos[0]
	}
	for _, a := range audios {
		var deps []string
		if ref != "" {
			deps = []string{ref}
		}
		if err := add(a, KindAudio, deps); err != nil {
			return nil, err
		}
	}
	all := append(append([]string(nil), videos...), audios...)
	if err := add("index", KindIndex, all); err != nil {
		return nil, err
	}
	p.Order = order
	if err := p.sortDeps(); err != nil {
		return nil, err
	}
	if err := p.computeLayers(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *Plan) sortDeps() error {
	for id, n := range p.Nodes {
		sort.Strings(n.Deps)
		for _, d := range n.Deps {
			if _, ok := p.Nodes[d]; !ok {
				return fmt.Errorf("graph: %q depends on unknown node %q", id, d)
			}
		}
		p.Nodes[id] = n
	}
	return nil
}

func (p *Plan) computeLayers() error {
	indeg := map[string]int{}
	children := map[string][]string{}
	for id, n := range p.Nodes {
		indeg[id] = len(n.Deps)
		for _, d := range n.Deps {
			children[d] = append(children[d], id)
		}
	}
	for k := range children {
		sort.Strings(children[k])
	}
	remaining := len(p.Nodes)
	for remaining > 0 {
		var layer []string
		for _, id := range p.Order {
			if indeg[id] == 0 {
				layer = append(layer, id)
			}
		}
		if len(layer) == 0 {
			return fmt.Errorf("graph: cycle detected with %d nodes remaining", remaining)
		}
		for _, id := range layer {
			indeg[id] = -1
			for _, c := range children[id] {
				if indeg[c] > 0 {
					indeg[c]--
				}
			}
		}
		sort.Strings(layer)
		p.Layers = append(p.Layers, layer)
		remaining -= len(layer)
	}
	return nil
}

// Ordered returns nodes in dependency order.
func (p *Plan) Ordered() []Node {
	out := make([]Node, 0, len(p.Order))
	for _, id := range p.Order {
		out = append(out, p.Nodes[id])
	}
	return out
}

// Validate checks that every dependency appears earlier in the order.
func (p *Plan) Validate() error {
	pos := map[string]int{}
	for i, id := range p.Order {
		pos[id] = i
	}
	for _, id := range p.Order {
		for _, d := range p.Nodes[id].Deps {
			if pos[d] >= pos[id] {
				return fmt.Errorf("graph: %q scheduled before dependency %q", id, d)
			}
		}
	}
	return nil
}
