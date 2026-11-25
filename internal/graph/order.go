// Package graph orders tracks by their dependency edges so that a track
// is processed only after every track it depends on.
package graph

import "fmt"

// CycleError reports a dependency cycle or unknown reference.
type CycleError struct{ Msg string }

func (e *CycleError) Error() string { return e.Msg }

// Order returns track IDs in dependency-first topological order.
// ids is the full set; deps maps id -> ids it depends on. Unknown
// dependency references and cycles are errors.
func Order(ids []string, deps map[string][]string) ([]string, error) {
	inSet := map[string]bool{}
	for _, id := range ids {
		inSet[id] = true
	}
	indeg := map[string]int{}
	children := map[string][]string{}
	for _, id := range ids {
		indeg[id] = 0
	}
	for _, id := range ids {
		for _, d := range deps[id] {
			if !inSet[d] {
				return nil, &CycleError{Msg: fmt.Sprintf("track %q depends on unknown track %q", id, d)}
			}
			indeg[id]++
			children[d] = append(children[d], id)
		}
	}
	// Kahn's algorithm; iterate ids in input order for determinism.
	var queue []string
	for _, id := range ids {
		if indeg[id] == 0 {
			queue = append(queue, id)
		}
	}
	var out []string
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		out = append(out, id)
		for _, c := range children[id] {
			indeg[c]--
			if indeg[c] == 0 {
				queue = append(queue, c)
			}
		}
	}
	if len(out) != len(ids) {
		return nil, &CycleError{Msg: "dependency cycle among tracks"}
	}
	return out, nil
}
