package graph

// scc computes strongly connected components with an iterative Tarjan pass, so
// a long dependency chain stays linear rather than quadratic.
func (g *Graph) scc() (map[string]int, int) {
	index := 0
	indices := map[string]int{}
	low := map[string]int{}
	onStack := map[string]bool{}
	var stack []string
	comp := map[string]int{}
	compCount := 0

	type frame struct {
		node string
		next int
	}
	for _, root := range g.order {
		if _, seen := indices[root]; seen {
			continue
		}
		callStack := []frame{{node: root}}
		for len(callStack) > 0 {
			f := &callStack[len(callStack)-1]
			cur := f.node
			if f.next == 0 {
				indices[cur] = index
				low[cur] = index
				index++
				stack = append(stack, cur)
				onStack[cur] = true
			}
			advanced := false
			for f.next < len(g.out[cur]) {
				e := g.out[cur][f.next]
				f.next++
				next := e.To
				if _, seen := indices[next]; !seen {
					callStack = append(callStack, frame{node: next})
					advanced = true
					break
				}
				if onStack[next] && indices[next] < low[cur] {
					low[cur] = indices[next]
				}
			}
			if advanced {
				continue
			}
			if low[cur] == indices[cur] {
				for {
					n := stack[len(stack)-1]
					stack = stack[:len(stack)-1]
					onStack[n] = false
					comp[n] = compCount
					if n == cur {
						break
					}
				}
				compCount++
			}
			callStack = callStack[:len(callStack)-1]
			if len(callStack) > 0 {
				parent := callStack[len(callStack)-1].node
				if low[cur] < low[parent] {
					low[parent] = low[cur]
				}
			}
		}
	}
	return comp, compCount
}
