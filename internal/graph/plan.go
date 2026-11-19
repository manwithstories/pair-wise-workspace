// Package graph orders recovery steps by track dependency. Video and audio
// tracks depend on their parameter-set source; media tracks depend on the
// index that bounds them. A missing dependency or a cycle stops only the
// affected branch and names every track involved.
package graph

import (
	"fmt"
	"sort"

	"archive.local/recover/internal/errs"
	"archive.local/recover/internal/model"
)

// Node is one recoverable track plus the tracks it requires.
type Node struct {
	ID    uint32
	Kind  string
	Deps  []uint32
	Fatal bool // set when the track's own input failed irrecoverably
}

// Graph is the input dependency graph.
type Graph struct {
	Nodes map[uint32]*Node
	File  string
}

// New builds a graph, validating that every node has a unique ID.
func New(nodes []Node, file string) (*Graph, error) {
	g := &Graph{Nodes: make(map[uint32]*Node, len(nodes)), File: file}
	for i := range nodes {
		n := nodes[i]
		if n.Kind == "" {
			n.Kind = model.KindOther
		}
		if _, dup := g.Nodes[n.ID]; dup {
			return nil, fmt.Errorf("duplicate track id %d", n.ID)
		}
		cp := n
		g.Nodes[n.ID] = &cp
	}
	return g, nil
}

// Plan is the ordered recovery schedule plus the branches that were stopped.
type Plan struct {
	Order     []uint32            `json:"order"`
	Stopped   []Stopped           `json:"stopped"`
	Issues    errs.List           `json:"issues"`
	DependsOn map[uint32][]uint32 `json:"depends_on"`
}

// Stopped records a branch that cannot be recovered and why.
type Stopped struct {
	TrackID uint32   `json:"track_id"`
	Kind    string   `json:"kind"`
	Reason  string   `json:"reason"`
	Code    string   `json:"code"`
	Related []uint32 `json:"related_tracks,omitempty"`
}

// Sort produces a deterministic topological order. Ties are broken by track ID
// so that runs are reproducible. Tracks reachable from a missing dependency or
// cycle are marked stopped; independent branches still proceed.
func (g *Graph) Sort() *Plan {
	p := &Plan{DependsOn: map[uint32][]uint32{}}
	ids := make([]uint32, 0, len(g.Nodes))
	for id := range g.Nodes {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	// Resolve missing dependencies first: those nodes and everything that
	// transitively needs them are stopped.
	blocked := map[uint32]string{}
	for _, id := range ids {
		n := g.Nodes[id]
		for _, d := range n.Deps {
			if _, ok := g.Nodes[d]; !ok {
				key := fmt.Sprintf("track %d depends on missing track %d", id, d)
				blocked[id] = key
				p.Issues.AddFatal(errs.ClassDependency, errs.CodeDependencyMissing, key,
					errs.Location{File: g.File, TrackID: id, Kind: n.Kind, Sample: -1, Offset: -1})
			}
		}
		if n.Fatal {
			if _, ok := blocked[id]; !ok {
				blocked[id] = fmt.Sprintf("track %d input is unrecoverable", id)
			}
		}
	}
	// Propagate blocking through dependants.
	for changed := true; changed; {
		changed = false
		for _, id := range ids {
			if _, isBlocked := blocked[id]; isBlocked {
				continue
			}
			for _, d := range g.Nodes[id].Deps {
				if reason, bad := blocked[d]; bad {
					blocked[id] = fmt.Sprintf("depends on stopped track %d (%s)", d, reason)
					p.Issues.AddFatal(errs.ClassDependency, errs.CodeDependencyStopped, blocked[id],
						errs.Location{File: g.File, TrackID: id, Kind: g.Nodes[id].Kind, Sample: -1, Offset: -1})
					changed = true
					break
				}
			}
		}
	}

	// Kahn's algorithm over the unblocked subgraph.
	indeg := map[uint32]int{}
	unblocked := make([]uint32, 0, len(ids))
	for _, id := range ids {
		p.DependsOn[id] = append([]uint32(nil), g.Nodes[id].Deps...)
		if _, bad := blocked[id]; bad {
			continue
		}
		unblocked = append(unblocked, id)
		indeg[id] = 0
	}
	for _, id := range unblocked {
		for _, d := range g.Nodes[id].Deps {
			if _, ok := indeg[d]; ok {
				indeg[id]++
			}
		}
	}
	inUnblocked := map[uint32]bool{}
	for _, id := range unblocked {
		inUnblocked[id] = true
	}
	for len(p.Order) < len(unblocked) {
		progress := false
		for _, id := range unblocked {
			if indeg[id] != 0 {
				continue
			}
			p.Order = append(p.Order, id)
			indeg[id] = -1
			progress = true
			for _, other := range unblocked {
				if indeg[other] <= 0 {
					continue
				}
				for _, d := range g.Nodes[other].Deps {
					if d == id {
						indeg[other]--
						break
					}
				}
			}
			break // restart scan to keep the smallest-ID-first tie break stable
		}
		if !progress {
			break
		}
	}
	// Anything unblocked but not ordered sits on a cycle. Find the cycle
	// members and stop them together, naming every track involved.
	if len(p.Order) < len(unblocked) {
		remaining := map[uint32]bool{}
		for _, id := range unblocked {
			if !contains(p.Order, id) {
				remaining[id] = true
			}
		}
		cycle := findCycle(g, remaining)
		related := make([]uint32, 0, len(cycle))
		for _, id := range cycle {
			related = append(related, id)
		}
		sort.Slice(related, func(i, j int) bool { return related[i] < related[j] })
		msg := fmt.Sprintf("dependency cycle among tracks %v", related)
		p.Issues.AddFatal(errs.ClassDependency, errs.CodeDependencyCycle, msg,
			errs.Location{File: g.File, TrackID: related[0], Sample: -1, Offset: -1})
		for id := range remaining {
			blocked[id] = msg
		}
		for _, id := range related {
			p.Stopped = append(p.Stopped, Stopped{
				TrackID: id, Kind: g.Nodes[id].Kind,
				Reason: msg, Code: errs.CodeDependencyCycle, Related: related,
			})
		}
	}
	// Emit stopped branches in a stable order.
	for _, id := range ids {
		reason, bad := blocked[id]
		if !bad {
			continue
		}
		if _, ok := g.Nodes[id]; !ok {
			continue
		}
		if isCycleMember(p.Stopped, id) {
			continue
		}
		code := errs.CodeDependencyStopped
		if n := g.Nodes[id]; n.Fatal {
			code = errs.CodeDependencyMissing
		}
		for _, d := range g.Nodes[id].Deps {
			if _, missing := g.Nodes[d]; !missing {
				code = errs.CodeDependencyMissing
			}
		}
		p.Stopped = append(p.Stopped, Stopped{
			TrackID: id, Kind: g.Nodes[id].Kind, Reason: reason, Code: code,
		})
	}
	sort.SliceStable(p.Stopped, func(i, j int) bool { return p.Stopped[i].TrackID < p.Stopped[j].TrackID })
	return p
}

// findCycle returns the members of one cycle in the remaining subgraph.
func findCycle(g *Graph, remaining map[uint32]bool) []uint32 {
	state := map[uint32]int{} // 0 unvisited, 1 on stack, 2 done
	var stack []uint32
	var found []uint32
	var visit func(uint32) bool
	visit = func(id uint32) bool {
		state[id] = 1
		stack = append(stack, id)
		for _, d := range g.Nodes[id].Deps {
			if !remaining[d] {
				continue
			}
			switch state[d] {
			case 1:
				// Slice the stack from the first occurrence of d.
				for i, s := range stack {
					if s == d {
						found = append(found, stack[i:]...)
						return true
					}
				}
			case 0:
				if visit(d) {
					return true
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[id] = 2
		return false
	}
	ids := make([]uint32, 0, len(remaining))
	for id := range remaining {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		if state[id] == 0 && visit(id) {
			break
		}
	}
	return found
}

func contains(list []uint32, v uint32) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func isCycleMember(st []Stopped, id uint32) bool {
	for _, s := range st {
		if s.TrackID == id {
			return true
		}
	}
	return false
}

// Recoverable reports whether a track is in the ordered, non-stopped set.
func (p *Plan) Recoverable(id uint32) bool {
	return contains(p.Order, id)
}
