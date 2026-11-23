// Package scheduler runs the archive pipeline as a dependency DAG. Nodes
// execute in dependency order; a failed node blocks its dependents and the
// scheduler reports the first blocking node with its dependency chain.
// After the failed node is fixed, Run can be called again: nodes that
// already succeeded are reused, only pending nodes re-execute.
package scheduler

import (
	"context"
	"fmt"
	"strings"

	"archive/internal/media"
)

// Node is one unit of pipeline work. Version is part of the idempotency
// key and must be bumped when the node's logic changes. Reentrant nodes
// are retried on failure; non-reentrant nodes fail immediately.
type Node struct {
	Name      string
	Deps      []string
	Version   string
	Reentrant bool
	Params    func() string // stable parameter summary for the idempotency key
	Run       func(ctx context.Context, deps map[string]any) (any, error)
}

// BlockedError reports the first node whose failure blocked the pipeline,
// with the dependency chain from that node to a terminal consumer.
type BlockedError struct {
	Node  string
	Chain []string
	Err   error
}

func (e *BlockedError) Error() string {
	return fmt.Sprintf("blocked_node=%s chain=%s: %v",
		e.Node, strings.Join(e.Chain, "->"), e.Err)
}

func (e *BlockedError) Unwrap() error { return e.Err }

// Scheduler executes a DAG of nodes with memoized results.
type Scheduler struct {
	nodes   map[string]*Node
	order   []string // insertion order, for deterministic scheduling
	results map[string]any
	done    map[string]bool // idempotency keys completed this process
	keys    map[string]string
}

// New creates an empty scheduler.
func New() *Scheduler {
	return &Scheduler{
		nodes: map[string]*Node{}, results: map[string]any{},
		done: map[string]bool{}, keys: map[string]string{},
	}
}

// Add registers a node.
func (s *Scheduler) Add(n *Node) {
	if _, dup := s.nodes[n.Name]; !dup {
		s.order = append(s.order, n.Name)
	}
	s.nodes[n.Name] = n
}

// Results returns the outputs of all succeeded nodes.
func (s *Scheduler) Results() map[string]any { return s.results }

// Run executes all nodes in dependency order. Nodes whose idempotency key
// is already done are skipped (resume semantics). On failure it returns a
// *BlockedError identifying the first blocking node and the chain.
func (s *Scheduler) Run(ctx context.Context) error {
	if err := s.validate(); err != nil {
		return err
	}
	var failed map[string]error
	for _, name := range s.order {
		n := s.nodes[name]
		deps := map[string]any{}
		depKeys := make([]string, 0, len(n.Deps))
		blocked := false
		for _, d := range n.Deps {
			if _, ok := s.results[d]; !ok {
				blocked = true // a dependency failed earlier
				break
			}
			deps[d] = s.results[d]
			depKeys = append(depKeys, s.keys[d])
		}
		if blocked {
			continue
		}
		_, err := s.exec(ctx, n, deps, depKeys)
		if err != nil {
			if failed == nil {
				failed = map[string]error{}
			}
			failed[name] = err
		}
	}
	if len(failed) > 0 {
		return s.blockedErr(failed)
	}
	return nil
}

// validate checks for unknown dependencies and cycles.
func (s *Scheduler) validate() error {
	for _, n := range s.nodes {
		for _, d := range n.Deps {
			if _, ok := s.nodes[d]; !ok {
				return media.New(media.KindInternal, "scheduler.dag", "", -1,
					"unknown dependency "+d+" of "+n.Name, nil)
			}
		}
	}
	// Iterative DFS cycle check.
	const (
		white, gray, black = 0, 1, 2
	)
	color := map[string]int{}
	var visit func(string) bool
	visit = func(u string) bool {
		color[u] = gray
		for _, d := range s.nodes[u].Deps {
			if color[d] == gray {
				return true
			}
			if color[d] == white && visit(d) {
				return true
			}
		}
		color[u] = black
		return false
	}
	for _, name := range s.order {
		if color[name] == white && visit(name) {
			return media.New(media.KindInternal, "scheduler.dag", "", -1,
				"dependency cycle involving "+name, nil)
		}
	}
	return nil
}

// blockedErr picks the first (in dependency order) failed node and builds
// the chain from it to a terminal dependent.
func (s *Scheduler) blockedErr(failed map[string]error) error {
	first := ""
	for _, name := range s.order {
		if _, ok := failed[name]; ok {
			first = name
			break
		}
	}
	chain := []string{first}
	cur := first
	for {
		next := ""
		for _, name := range s.order {
			for _, d := range s.nodes[name].Deps {
				if d == cur {
					next = name
					break
				}
			}
			if next != "" {
				break
			}
		}
		if next == "" {
			break
		}
		chain = append(chain, next)
		cur = next
	}
	return &BlockedError{Node: first, Chain: chain, Err: failed[first]}
}
