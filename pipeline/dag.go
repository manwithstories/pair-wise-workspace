// Package pipeline schedules nodes with dependencies as a DAG: concurrent
// execution, failure propagation, bounded retries, and checkpointed state
// so a restarted process only re-runs incomplete nodes.
package pipeline

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"bcast/media"
)

// State is the lifecycle state of a node.
type State string

const (
	StatePending   State = "pending"
	StateDone      State = "done"
	StateFailed    State = "failed"
	StateBlocked   State = "blocked"
	StateRecovered State = "recovered" // done, via retry or degraded input
)

// Node is one unit of work. Run returns a result summary (e.g. a hash)
// stored under the node's idempotency key.
type Node struct {
	ID         string
	Deps       []string
	Key        string // idempotency key; "" disables result reuse
	MaxRetries int
	Run        func(ctx context.Context) (string, error)
}

// NodeState is the persisted state of one node.
type NodeState struct {
	State     State
	Key       string // idempotency key the state belongs to
	Attempts  int
	Result    string
	Err       string
	Dep       string // root dependency named by a blocking failure
	Recovered bool   // completed via retry, resume or degraded input
}

// DAG is a set of nodes plus their persisted states.
type DAG struct {
	nodes  map[string]*Node
	order  []string
	States map[string]*NodeState
	cp     *Checkpoint
	mu     sync.Mutex
}

// New builds an empty DAG persisting to cp (nil cp = in-memory only).
func New(cp *Checkpoint) *DAG {
	if cp == nil {
		cp = NewCheckpoint("")
	}
	return &DAG{nodes: map[string]*Node{}, States: map[string]*NodeState{}, cp: cp}
}

// Add registers a node. A node whose checkpoint state is terminal *and* was
// produced under the same idempotency key keeps its stored state and does not
// re-run. If the key changed — different input or parameters — the node is
// reset to pending, because the old result does not describe this task.
func (d *DAG) Add(n Node) error {
	if _, dup := d.nodes[n.ID]; dup {
		return fmt.Errorf("pipeline: duplicate node %q", n.ID)
	}
	node := n
	d.nodes[node.ID] = &node
	d.order = append(d.order, node.ID)
	if saved, ok := d.cp.Nodes[node.ID]; ok && saved.Key == node.Key &&
		(saved.State == StateDone || saved.State == StateRecovered) {
		s := saved
		s.Recovered = s.Recovered || saved.State == StateRecovered
		d.States[node.ID] = &s
		return nil
	}
	d.States[node.ID] = &NodeState{State: StatePending, Key: node.Key}
	return nil
}

// Resumed reports how many nodes were reused from the checkpoint.
func (d *DAG) Resumed() int {
	n := 0
	for _, id := range d.order {
		st := d.States[id]
		if st != nil && (st.State == StateDone || st.State == StateRecovered) && st.Attempts == 0 {
			n++
		}
	}
	return n
}

// Execute runs the DAG to quiescence with up to workers concurrent nodes.
func (d *DAG) Execute(ctx context.Context, workers int) *Report {
	if workers < 1 {
		workers = 1
	}
	for {
		ready := d.ready()
		if len(ready) == 0 {
			break
		}
		sem := make(chan struct{}, workers)
		var wg sync.WaitGroup
		for _, id := range ready {
			if ctx.Err() != nil {
				break
			}
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				d.runNode(ctx, id)
			}(id)
		}
		wg.Wait()
		if ctx.Err() != nil {
			break
		}
	}
	// Anything still pending could not run: a dependency failed or was
	// itself blocked. Name the root cause, not the immediate parent.
	for _, id := range d.order {
		st := d.States[id]
		if st.State != StatePending {
			continue
		}
		st.State = StateBlocked
		st.Dep = d.rootFailure(id)
		if st.Dep != "" {
			st.Err = fmt.Sprintf("blocked by %s", st.Dep)
		} else {
			st.Err = "blocked: context cancelled"
		}
	}
	d.persist()
	return d.report()
}

// ready returns pending nodes whose dependencies all completed.
func (d *DAG) ready() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, id := range d.order {
		st := d.States[id]
		if st == nil || st.State != StatePending {
			continue
		}
		ok := true
		for _, dep := range d.nodes[id].Deps {
			s := d.States[dep]
			if s == nil || (s.State != StateDone && s.State != StateRecovered) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, id)
		}
	}
	return out
}

// runNode executes one node with idempotent retry and records the outcome.
func (d *DAG) runNode(ctx context.Context, id string) {
	n := d.nodes[id]
	result, attempts, recovered, err := RunIdempotent(ctx, d.cp, n.Key, n.MaxRetries, n.Run)
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.States[id]
	st.Attempts += attempts
	st.Result = result
	st.Recovered = recovered
	switch {
	case err == nil && recovered:
		st.State = StateRecovered
	case err == nil:
		st.State = StateDone
	default:
		st.State = StateFailed
		st.Err = err.Error()
		st.Dep = media.FirstBlockingDep(err)
		if st.Dep == "" && media.ClassOf(err) == media.ClassBlocked {
			st.Dep = id
		}
	}
	d.persistLocked()
}

// persist flushes node states into the checkpoint and saves it.
func (d *DAG) persist() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.persistLocked()
}

// persistLocked writes states through to the checkpoint. Callers hold d.mu.
func (d *DAG) persistLocked() {
	d.cp.mu.Lock()
	for id, st := range d.States {
		d.cp.Nodes[id] = *st
	}
	d.cp.mu.Unlock()
	d.cp.Save()
}

// rootFailure returns the earliest failed or blocked ancestor of id, walking
// dependencies in declaration order. This is the first blocking dependency
// an operator should investigate.
func (d *DAG) rootFailure(id string) string {
	seen := map[string]bool{}
	var walk func(string) string
	walk = func(node string) string {
		if seen[node] {
			return ""
		}
		seen[node] = true
		n := d.nodes[node]
		if n == nil {
			return ""
		}
		for _, dep := range n.Deps {
			st := d.States[dep]
			if st == nil {
				continue
			}
			if st.State == StateFailed {
				return dep
			}
			if st.State == StateBlocked {
				if root := walk(dep); root != "" {
					return root
				}
				return dep
			}
		}
		return ""
	}
	return walk(id)
}

// Report summarizes one Execute run.
type Report struct {
	FirstBlockedDep string
	RecoveredNodes  int
	ResumedNodes    int
	FailedNodes     []string
	BlockedNodes    []string
	DoneNodes       int
	TotalNodes      int
}

func (d *DAG) report() *Report {
	d.mu.Lock()
	defer d.mu.Unlock()
	r := &Report{TotalNodes: len(d.order), ResumedNodes: d.Resumed()}
	for _, id := range d.order {
		st := d.States[id]
		switch st.State {
		case StateDone:
			r.DoneNodes++
		case StateRecovered:
			r.DoneNodes++
			r.RecoveredNodes++
		case StateFailed:
			r.FailedNodes = append(r.FailedNodes, id)
		case StateBlocked:
			r.BlockedNodes = append(r.BlockedNodes, id)
		}
		if r.FirstBlockedDep == "" && st.Dep != "" &&
			(st.State == StateFailed || st.State == StateBlocked) {
			r.FirstBlockedDep = st.Dep
		}
	}
	sort.Strings(r.FailedNodes)
	sort.Strings(r.BlockedNodes)
	if len(r.FailedNodes) > 0 && r.FirstBlockedDep == "" {
		r.FirstBlockedDep = r.FailedNodes[0]
	}
	return r
}

// OK reports whether every node completed (recoveries allowed).
func (r *Report) OK() bool {
	return len(r.FailedNodes) == 0 && len(r.BlockedNodes) == 0
}
