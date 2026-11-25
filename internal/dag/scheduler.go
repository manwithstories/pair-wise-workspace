// Package dag schedules the parse/mux/checkpoint pipeline as a dependency
// DAG. Each node has an idempotency key derived from its input hash and
// parameters, so retries and resumed runs never write twice. When a
// dependency fails, the first blocked node is reported in topological
// order.
package dag

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"

	"mediad/internal/fault"
)

// Node is one unit of work in the pipeline.
type Node struct {
	ID         string
	Deps       []string
	Params     string // serialized parameters, part of the idempotency key
	Input      []byte // input content, part of the idempotency key
	MaxRetries int
	Run        func() error
}

// Key is the idempotency key: SHA-256 over params and input content.
func (n *Node) Key() string {
	h := sha256.New()
	h.Write([]byte(n.Params))
	h.Write([]byte{0})
	h.Write(n.Input)
	return hex.EncodeToString(h.Sum(nil))
}

// Scheduler executes nodes in topological order.
type Scheduler struct {
	nodes     map[string]*Node
	Completed map[string]bool // idempotency keys already done (checkpoint)
}

// New builds a Scheduler; completed may come from a checkpoint store.
func New(nodes []*Node, completed map[string]bool) *Scheduler {
	if completed == nil {
		completed = map[string]bool{}
	}
	s := &Scheduler{nodes: map[string]*Node{}, Completed: completed}
	for _, n := range nodes {
		s.nodes[n.ID] = n
	}
	return s
}

// Report summarizes one Run.
type Report struct {
	Completed    []string `json:"completed"`
	Skipped      []string `json:"skipped"` // idempotent hits, not re-executed
	Retries      int      `json:"retries"`
	Failed       []string `json:"failed,omitempty"`
	FirstBlocked string   `json:"first_blocked,omitempty"`
	BlockedBy    []string `json:"blocked_by,omitempty"`
}

// Run executes the DAG. It returns the report even on failure so callers
// can inspect retries and the first blocked node; the error is a
// *fault.Error carrying KindCycle, KindRetry or KindBlocked.
func (s *Scheduler) Run() (Report, error) {
	var rep Report
	order, err := s.topo()
	if err != nil {
		return rep, err
	}
	failed := map[string]bool{}
	blocked := map[string]bool{}
	for _, id := range order {
		n := s.nodes[id]
		var badDeps []string
		for _, d := range n.Deps {
			if failed[d] || blocked[d] {
				badDeps = append(badDeps, d)
			}
		}
		if len(badDeps) > 0 {
			blocked[id] = true
			if rep.FirstBlocked == "" {
				rep.FirstBlocked = id
				rep.BlockedBy = badDeps
			}
			continue
		}
		key := n.Key()
		if s.Completed[key] {
			rep.Skipped = append(rep.Skipped, id)
			continue
		}
		var runErr error
		for attempt := 0; attempt <= n.MaxRetries; attempt++ {
			if attempt > 0 {
				rep.Retries++
			}
			if runErr = n.Run(); runErr == nil {
				break
			}
		}
		if runErr != nil {
			failed[id] = true
			rep.Failed = append(rep.Failed, id)
			continue
		}
		s.Completed[key] = true
		rep.Completed = append(rep.Completed, id)
	}
	if rep.FirstBlocked != "" {
		return rep, fault.New(fault.KindBlocked, rep.FirstBlocked,
			"blocked by failed dependencies").WithBlockedBy(rep.BlockedBy...)
	}
	if len(rep.Failed) > 0 {
		return rep, fault.New(fault.KindRetry, rep.Failed[0],
			"retries exhausted for %v", rep.Failed)
	}
	return rep, nil
}

// topo returns node IDs in topological order (Kahn, ties broken by ID for
// determinism) or a KindCycle error naming the cycle members.
func (s *Scheduler) topo() ([]string, error) {
	indeg := map[string]int{}
	for id, n := range s.nodes {
		indeg[id] += 0
		for _, d := range n.Deps {
			if _, ok := s.nodes[d]; !ok {
				return nil, fault.New(fault.KindBlocked, id, "unknown dependency %q", d)
			}
			indeg[id]++
		}
	}
	var ready []string
	for id, d := range indeg {
		if d == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	var order []string
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		for other, n := range s.nodes {
			for _, d := range n.Deps {
				if d == id {
					indeg[other]--
					if indeg[other] == 0 {
						ready = append(ready, other)
						sort.Strings(ready)
					}
				}
			}
		}
	}
	if len(order) != len(s.nodes) {
		var cyc []string
		for id, d := range indeg {
			if d > 0 {
				cyc = append(cyc, id)
			}
		}
		sort.Strings(cyc)
		return nil, fault.New(fault.KindCycle, "", "dependency cycle among %v", cyc)
	}
	return order, nil
}
