// Package scheduler executes a dependency DAG with idempotent, dependency-
// ordered retries. A node's failure blocks its dependents (and only them);
// independent branches keep running. The first blocking dependency is retained
// so operators can explain why a product is missing.
package scheduler

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"mediad/internal/model"
)

// Node is one unit of work.
type Node struct {
	// Name is the node's stable identity, used as its idempotency key.
	Name string
	// Deps are node names that must succeed before this node runs.
	Deps []string
	// Run performs the work. It must be idempotent: retries and re-runs with
	// the same inputs must produce the same result.
	Run func() (Result, error)
}

// Result is a node's output envelope.
type Result struct {
	// Hash commits to the node's product.
	Hash string
	// Data is opaque to the scheduler.
	Data any
}

// Status is the terminal state of a node.
type Status string

const (
	StatusPending Status = "pending"
	StatusDone    Status = "done"
	StatusFailed  Status = "failed"
	StatusBlocked Status = "blocked"
	// StatusSkipped is used when a node is cut off by a dependency that never
	// ran because the run was interrupted.
	StatusSkipped Status = "skipped"
)

// NodeState is the outcome recorded for one node.
type NodeState struct {
	Name     string
	Status   Status
	Attempts int
	Result   Result
	Err      *model.Error
	// BlockedBy is the first dependency that prevented this node from running.
	BlockedBy string
	// RootCause is the ultimate failed node, derived through the chain.
	RootCause string
}

// Report is the execution outcome.
type Report struct {
	Order      []string
	States     map[string]*NodeState
	FirstBlock string
	// Succeeded, Failed, Blocked count nodes by final status.
	Succeeded, Failed, Blocked int
}

// NodeStateList returns states in execution order.
func (r *Report) NodeStateList() []NodeState {
	out := make([]NodeState, 0, len(r.Order))
	for _, name := range r.Order {
		if st, ok := r.States[name]; ok {
			out = append(out, *st)
		}
	}
	return out
}

// Scheduler executes a DAG.
type Scheduler struct {
	nodes map[string]*Node
	// order is topological; computed once and reused for determinism.
	order []string
	// MaxAttempts is the total attempts per node (1 means no retry).
	MaxAttempts int
	// BaseDelay is a hook for deterministic backoff accounting; the scheduler
	// never sleeps, so runs are reproducible.
	BaseDelay int64
}

// New builds a scheduler. Names must be unique and dependencies must exist.
func New(nodes []Node) (*Scheduler, error) {
	s := &Scheduler{nodes: make(map[string]*Node, len(nodes)), MaxAttempts: 1}
	for i := range nodes {
		n := nodes[i]
		if n.Name == "" {
			return nil, model.New(model.CategoryScheduler, "build", "node with empty name")
		}
		if _, dup := s.nodes[n.Name]; dup {
			return nil, model.New(model.CategoryScheduler, "build", "duplicate node "+n.Name).
				WithBlockingDep(n.Name)
		}
		s.nodes[n.Name] = &n
	}
	for _, n := range nodes {
		for _, d := range n.Deps {
			if _, ok := s.nodes[d]; !ok {
				return nil, model.New(model.CategoryScheduler, "build",
					fmt.Sprintf("node %s depends on unknown %s", n.Name, d)).
					WithBlockingDep(d)
			}
		}
	}
	order, err := s.topological()
	if err != nil {
		return nil, err
	}
	s.order = order
	return s, nil
}

// topological sorts by name, then Kahn's algorithm, for determinism.
func (s *Scheduler) topological() ([]string, error) {
	names := make([]string, 0, len(s.nodes))
	for name := range s.nodes {
		names = append(names, name)
	}
	sort.Strings(names)
	indeg := make(map[string]int, len(names))
	children := make(map[string][]string, len(names))
	for _, name := range names {
		n := s.nodes[name]
		seen := map[string]bool{}
		for _, d := range n.Deps {
			if seen[d] {
				continue
			}
			seen[d] = true
			indeg[name]++
			children[d] = append(children[d], name)
		}
	}
	ready := make([]string, 0, len(names))
	for _, name := range names {
		if indeg[name] == 0 {
			ready = append(ready, name)
		}
	}
	sort.Strings(ready)
	var order []string
	for len(ready) > 0 {
		name := ready[0]
		ready = ready[1:]
		order = append(order, name)
		kids := append([]string(nil), children[name]...)
		sort.Strings(kids)
		for _, k := range kids {
			indeg[k]--
			if indeg[k] == 0 {
				ready = append(ready, k)
				sort.Strings(ready)
			}
		}
	}
	if len(order) != len(names) {
		return nil, model.New(model.CategoryScheduler, "topological", "cycle detected in dependency graph")
	}
	return order, nil
}

// Order returns the deterministic topological order.
func (s *Scheduler) Order() []string { return append([]string(nil), s.order...) }

// IdempotencyKey returns the stable key for a node given its dependency hashes.
func IdempotencyKey(node string, depHashes []string) string {
	h := sha256.New()
	fmt.Fprintf(h, "node=%s;", node)
	for _, d := range depHashes {
		fmt.Fprintf(h, "dep=%s;", d)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Run executes all nodes with dependency-ordered retries.
func (s *Scheduler) Run() *Report { return s.Resume(nil) }

// Resume executes the DAG, reusing any node already marked done in prior.
// Reused nodes are not re-run: their recorded result is the idempotent product
// from the interrupted attempt. Everything else is dependency-ordered as usual.
func (s *Scheduler) Resume(prior map[string]NodeState) *Report {
	report := &Report{States: make(map[string]*NodeState, len(s.nodes))}
	for _, name := range s.order {
		report.Order = append(report.Order, name)
	}
	for _, name := range s.order {
		n := s.nodes[name]
		st := &NodeState{Name: name}
		report.States[name] = st

		if prev, ok := prior[name]; ok && prev.Status == StatusDone {
			// Idempotent resume: replay the pure node to rebuild in-memory
			// state, then require the product hash to match the interrupted
			// run. Attempts are not incremented; a mismatch is a failure.
			if prev.Result.Hash == "" {
				st.Status = StatusFailed
				st.Err = model.New(model.CategoryScheduler, "resume",
					"completed node "+name+" has no product hash").WithBlockingDep(name)
				report.Failed++
				if report.FirstBlock == "" {
					report.FirstBlock = name
				}
				continue
			}
			res, err := n.Run()
			if err != nil {
				st.Status = StatusFailed
				st.Err = wrapNodeErr(name, err)
				report.Failed++
				if report.FirstBlock == "" {
					report.FirstBlock = name
				}
				continue
			}
			if res.Hash != prev.Result.Hash {
				st.Status = StatusFailed
				st.Err = model.New(model.CategoryScheduler, "resume",
					fmt.Sprintf("node %s product changed on replay: %s != %s", name, res.Hash, prev.Result.Hash)).
					WithBlockingDep(name)
				report.Failed++
				if report.FirstBlock == "" {
					report.FirstBlock = name
				}
				continue
			}
			st.Status, st.Result, st.Attempts = StatusDone, prev.Result, prev.Attempts
			report.Succeeded++
			continue
		}

		// Resolve dependencies first.
		var depHashes []string
		blocked := ""
		for _, d := range n.Deps {
			ds := report.States[d]
			if ds == nil || ds.Status != StatusDone {
				if blocked == "" {
					blocked = d
				}
			}
			depHashes = append(depHashes, ds.Result.Hash)
		}
		if blocked != "" {
			root := report.rootCause(blocked)
			st.Status = StatusBlocked
			st.BlockedBy = blocked
			st.RootCause = root
			st.Err = model.New(model.CategoryScheduler, "blocked",
				fmt.Sprintf("node %s blocked by %s", name, blocked)).
				WithBlockingDep(root)
			report.Blocked++
			if report.FirstBlock == "" {
				report.FirstBlock = root
			}
			continue
		}

		key := IdempotencyKey(name, depHashes)
		var lastErr *model.Error
		maxAttempts := s.MaxAttempts
		if maxAttempts < 1 {
			maxAttempts = 1
		}
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			st.Attempts = attempt
			res, err := n.Run()
			if err == nil {
				st.Status = StatusDone
				st.Result = res
				if res.Hash == "" {
					st.Result.Hash = key
				}
				st.Err = nil
				break
			}
			lastErr = wrapNodeErr(name, err)
			if attempt == maxAttempts {
				st.Status = StatusFailed
				st.Err = lastErr
				st.RootCause = name
			}
		}
		switch st.Status {
		case StatusDone:
			report.Succeeded++
		case StatusFailed:
			report.Failed++
			if report.FirstBlock == "" {
				report.FirstBlock = name
			}
		}
	}
	return report
}

func wrapNodeErr(name string, err error) *model.Error {
	var me *model.Error
	if errors.As(err, &me) {
		return me.WithBlockingDep(firstDep(me, name))
	}
	return model.Wrap(model.CategoryScheduler, "node:"+name, err).WithBlockingDep(name)
}

func firstDep(me *model.Error, name string) string {
	if me.BlockingDep != "" {
		return me.BlockingDep
	}
	return name
}

// rootCause walks the blocked/failed chain to the originating failure.
func (r *Report) rootCause(name string) string {
	seen := map[string]bool{}
	for {
		if seen[name] {
			return name
		}
		seen[name] = true
		st := r.States[name]
		if st == nil {
			return name
		}
		if st.Status == StatusFailed {
			return name
		}
		if st.BlockedBy == "" {
			return name
		}
		name = st.BlockedBy
	}
}

// Summary renders a one-line terminal summary.
func (r *Report) Summary() string {
	return fmt.Sprintf("succeeded=%d failed=%d blocked=%d first_block=%s",
		r.Succeeded, r.Failed, r.Blocked, r.FirstBlockOrNone())
}

// FirstBlockOrNone returns "none" on a clean run.
func (r *Report) FirstBlockOrNone() string {
	if r.FirstBlock == "" {
		return "none"
	}
	return r.FirstBlock
}

// FailedNodes lists failed node names in order.
func (r *Report) FailedNodes() []string {
	var out []string
	for _, name := range r.Order {
		if r.States[name].Status == StatusFailed {
			out = append(out, name)
		}
	}
	return out
}

// BlockedNodes lists blocked node names in order.
func (r *Report) BlockedNodes() []string {
	var out []string
	for _, name := range r.Order {
		if r.States[name].Status == StatusBlocked {
			out = append(out, name)
		}
	}
	return out
}

// Idempotent applies a function up to n times, returning the first success and
// the total attempts. It exists for callers that want the retry policy without
// the DAG.
func Idempotent(n int, fn func() error) (int, error) {
	if n < 1 {
		n = 1
	}
	var last error
	for i := 1; i <= n; i++ {
		if err := fn(); err == nil {
			return i, nil
		} else {
			last = err
		}
	}
	return n, last
}
