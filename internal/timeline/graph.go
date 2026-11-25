package timeline

import (
	"fmt"
	"sort"
	"strings"
)

// Node is one deterministic recovery pipeline stage and its prerequisites.
type Node struct {
	Name string
	Deps []string
}

// DefaultGraph encodes the required order: initialization metadata, sample
// indexing, cross-track event mapping, remux, and then manifest production.
func DefaultGraph() []Node {
	return []Node{
		{Name: "init", Deps: nil},
		{Name: "sample-index", Deps: []string{"init"}},
		{Name: "event-map", Deps: []string{"sample-index"}},
		{Name: "remux", Deps: []string{"event-map"}},
		{Name: "manifest", Deps: []string{"remux"}},
	}
}

// TopoSort returns node names in dependency order. Ties are broken by name so
// repeated runs and resume journals are byte-stable.
func TopoSort(nodes []Node) ([]string, error) {
	byName := make(map[string]Node, len(nodes))
	for _, n := range nodes {
		if _, dup := byName[n.Name]; dup {
			return nil, New(CodeInvalidArgument, "graph", "duplicate node "+n.Name)
		}
		byName[n.Name] = n
	}
	for _, n := range nodes {
		for _, dep := range n.Deps {
			if _, ok := byName[dep]; !ok {
				return nil, &Error{Code: CodeMissingDependency, Op: "graph", Offset: -1,
					Detail: fmt.Sprintf("node %q requires missing %q", n.Name, dep)}
			}
		}
	}

	indegree := make(map[string]int, len(nodes))
	remaining := make(map[string][]string, len(nodes))
	for _, n := range nodes {
		indegree[n.Name] = len(n.Deps)
		for _, dep := range n.Deps {
			remaining[dep] = append(remaining[dep], n.Name)
		}
	}
	ready := make([]string, 0)
	for name, d := range indegree {
		if d == 0 {
			ready = append(ready, name)
		}
	}
	sort.Strings(ready)
	order := make([]string, 0, len(nodes))
	for len(ready) > 0 {
		name := ready[0]
		ready = ready[1:]
		order = append(order, name)
		newReady := make([]string, 0)
		for _, next := range remaining[name] {
			indegree[next]--
			if indegree[next] == 0 {
				newReady = append(newReady, next)
			}
		}
		sort.Strings(newReady)
		ready = append(ready, newReady...)
		sort.Strings(ready)
	}
	if len(order) != len(nodes) {
		cycle := make([]string, 0)
		for name, d := range indegree {
			if d > 0 {
				cycle = append(cycle, name)
			}
		}
		sort.Strings(cycle)
		return nil, New(CodeCycle, "graph", "cycle involving "+strings.Join(cycle, ","))
	}
	return order, nil
}

// ValidateDefaultGraph is a small helper used by the CLI acceptance path.
func ValidateDefaultGraph() ([]string, error) {
	return TopoSort(DefaultGraph())
}
