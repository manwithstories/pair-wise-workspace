package graph

import (
	"sort"
)

// Cycles reports the dependency cycles in the graph. Each strongly connected
// component with more than one node, or a node with a self-edge, is one cycle;
// the returned path starts and ends at the component's first node in insertion
// order. Reporting one path per component is deliberate: a component with many
// elementary cycles would otherwise produce a combinatorial number of paths,
// and the located component is what an operator needs to act on.
func (g *Graph) Cycles() [][]string {
	comp, count := g.scc()
	byComp := make([][]string, count)
	for _, id := range g.order {
		byComp[comp[id]] = append(byComp[comp[id]], id)
	}
	var out [][]string
	for i, members := range byComp {
		selfLoop := false
		for _, e := range g.out[members[0]] {
			if e.To == members[0] {
				selfLoop = true
			}
		}
		if len(members) < 2 && !selfLoop {
			continue
		}
		path := g.cyclePath(comp, i, members)
		out = append(out, path)
	}
	return out
}

// cyclePath finds one concrete cycle inside a component. Every node in a
// nontrivial component lies on a cycle, so a depth-first walk from the first
// member that only follows intra-component edges must close.
func (g *Graph) cyclePath(comp map[string]int, id int, members []string) []string {
	start := members[0]
	inComp := map[string]bool{}
	for _, m := range members {
		inComp[m] = true
	}
	var path []string
	onPath := map[string]bool{}
	var found []string
	var walk func(cur string) bool
	walk = func(cur string) bool {
		path = append(path, cur)
		onPath[cur] = true
		adj := append([]Edge(nil), g.out[cur]...)
		sort.Slice(adj, func(i, j int) bool { return adj[i].To < adj[j].To })
		for _, e := range adj {
			if !inComp[e.To] {
				continue
			}
			if e.To == start {
				found = append(append([]string(nil), path...), start)
				return true
			}
			if onPath[e.To] {
				continue
			}
			if walk(e.To) {
				return true
			}
		}
		path = path[:len(path)-1]
		delete(onPath, cur)
		return false
	}
	if walk(start) {
		return found
	}
	// Self-loop or a component whose only return edge is to a later member.
	for _, e := range g.out[start] {
		if e.To == start {
			return []string{start, start}
		}
	}
	return append(append([]string(nil), members...), members[0])
}
