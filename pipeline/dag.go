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
	Attempts  int
	Result    string
	Err       string
	Dep       string // dependency named by a blocking/recovered failure
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

// Add registers a node. A node already done in the checkpoint keeps its
// stored state and will not re-run.
func (d *DAG) Add(n Node) {
	node := n
	d.nodes[node.ID] = &node
	d.order = append(d.order, node.ID)
	if saved, ok := d.cp.Nodes[node.ID]; ok && (saved.State == StateDone || saved.State == StateRecovered) {
		s := saved
		d.States[node.ID] = &s
		return
	}
	d.States[node.ID] = &NodeState{State: StatePending}
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
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				d.runNode(ctx, id)
			}(id)
		}
		wg.Wait()
	}
	// Anything still pending has a failed or blocked ancestor.
	for _, id := range d.order {
		st := d.States[id]
		if st.State != StatePending {
			continue
		}
		st.State = StateBlocked
		st.Dep = d.firstFailedDep(id)
		st.Err = fmt.Sprintf("blocked by %s", st.Dep)
	}
	d.persist()
	return d.report()
}

// ready returns pending nodes whose dependencies all completed.
func (d *DAG) ready() []string {
	var out []string
	for _, id := range d.order {
		if d.States[id].State != StatePending {
			continue
		}
		ok := true
		for _, dep := range d.nodes[id].Deps {
			s := d.States[dep].State
			if s != StateDone && s != StateRecovered {
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
	st := d.States[id]
	hadPriorAttempt := st.Attempts > 0
	result, attempts, recovered, err := RunIdempotent(ctx, d.cp, n.Key, n.MaxRetries, n.Run)
	d.mu.Lock()
	defer d.mu.Unlock()
	st.Attempts += attempts
	st.Result = result
	switch {
	case err == nil:
		st.State = StateDone
		st.Recovered = recovered || hadPriorAttempt
		if st.Recovered {
			st.State = StateRecovered
		}
	default:
		st.State = StateFailed
		st.Err = err.Error()
		st.Dep = media.FirstBlockingDep(err)
	}
	d.persist()
}

// persist flushes node states into the checkpoint and saves it.
// Callers must hold d.mu.
func (d *DAG) persist() {
	d.cp.mu.Lock()
	for id, st := range d.States {
		d.cp.Nodes[id] = *st
	}
	d.cp.mu.Unlock()
	d.cp.Save()
}

// firstFailedDep names the first failed dependency of id in declaration
// order — the first blocking dependency downstream care about.
func (d *DAG) firstFailedDep(id string) string {
	for _, dep := range d.nodes[id].Deps {
		if d.States[dep].State == StateFailed || d.States[dep].State == StateBlocked {
			return dep
		}
	}
	return ""
}

// Report summarizes one Execute run.
type Report struct {
	FirstBlockedDep string
	RecoveredNodes  int
	FailedNodes     []string
	BlockedNodes    []string
	DoneNodes       int
}

func (d *DAG) report() *Report {
	r := &Report{}
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
		if st.Dep != "" && r.FirstBlockedDep == "" {
			r.FirstBlockedDep = st.Dep
		}
	}
	sort.Strings(r.FailedNodes)
	sort.Strings(r.BlockedNodes)
	return r
}

// OK reports whether every node completed (recoveries allowed).
func (r *Report) OK() bool {
	return len(r.FailedNodes) == 0 && len(r.BlockedNodes) == 0
}
