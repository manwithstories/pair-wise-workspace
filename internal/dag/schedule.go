package dag

import (
	"fmt"
	"sort"

	"review/internal/media"
)

// RunAll executes nodes in dependency order. Already-succeeded nodes whose
// key still matches are skipped, giving safe resume after interruption.
// Nodes are topologically sorted (Kahn's algorithm) with names as the
// tie-break, so every dependency runs before its dependents.
func (rt *Runner) RunAll() error {
	order, err := rt.topoOrder()
	if err != nil {
		return err
	}
	// Pre-pass: mark every node whose dependency is already failed (from a
	// prior run) as blocked, wrapping the original error chain. This keeps
	// fail-fast semantics for fresh failures while surfacing blocked
	// status and provenance on resume.
	if err := rt.markBlockedDependents(); err != nil {
		return err
	}
	for _, name := range order {
		if err := rt.runOne(name); err != nil {
			return err
		}
	}
	return nil
}

// markBlockedDependents walks the graph and blocks nodes reachable from a
// failed dependency, replaying the durable error through BlockedBy.
func (rt *Runner) markBlockedDependents() error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	// Resolve the effective status of every node, propagating failure
	// transitively through the dependency edges.
	blockedErr := map[string]*media.PipelineError{}
	var resolve func(name string) *media.PipelineError
	resolve = func(name string) *media.PipelineError {
		if e, ok := blockedErr[name]; ok {
			return e
		}
		node := rt.nodes[name]
		st := rt.state[name]
		if st != nil && st.Status == StatusFailed {
			if cause, ok := rt.lastError[name].(*media.PipelineError); ok {
				blockedErr[name] = cause
				return cause
			}
			cause := media.NewInternal(name, st.Error, nil)
			rt.lastError[name] = cause
			blockedErr[name] = cause
			return cause
		}
		for _, dep := range node.Deps {
			if cause := resolve(dep); cause != nil {
				wrapped := media.BlockedBy(name, cause)
				blockedErr[name] = wrapped
				if st != nil {
					st.Status = StatusBlocked
					st.Error = wrapped.Error()
				}
				return wrapped
			}
		}
		return nil
	}
	var first *media.PipelineError
	for name := range rt.nodes {
		st := rt.state[name]
		e := resolve(name)
		if e != nil && st != nil && st.Status == StatusBlocked && first == nil {
			first = e
		}
	}
	if first != nil {
		_ = rt.persistLocked()
		return first
	}
	return nil
}

// topoOrder returns a deterministic execution order satisfying every
// dependency edge; names break ties so runs are reproducible.
func (rt *Runner) topoOrder() ([]string, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	indegree := map[string]int{}
	adj := map[string][]string{}
	var ready []string
	for name, node := range rt.nodes {
		indegree[name] = len(node.Deps)
		for _, dep := range node.Deps {
			if _, ok := rt.nodes[dep]; !ok {
				return nil, media.NewConflict(name,
					fmt.Sprintf("unknown dependency %q", dep), dep)
			}
			adj[dep] = append(adj[dep], name)
		}
		if len(node.Deps) == 0 {
			ready = append(ready, name)
		}
	}
	sort.Strings(ready)
	var order []string
	for len(ready) > 0 {
		name := ready[0]
		ready = ready[1:]
		order = append(order, name)
		followers := append([]string(nil), adj[name]...)
		sort.Strings(followers)
		for _, next := range followers {
			indegree[next]--
			if indegree[next] == 0 {
				ready = append(ready, next)
				sort.Strings(ready)
			}
		}
	}
	if len(order) != len(rt.nodes) {
		return nil, media.NewInternal("dag-runner", "dependency cycle detected", nil)
	}
	return order, nil
}
