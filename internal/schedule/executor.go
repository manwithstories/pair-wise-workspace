package schedule

import (
	"errors"
	"fmt"
	"sync"

	"mediac/internal/core"
)

// Config controls a run.
type Config struct {
	Workers  int
	MaxRetry int
}

// RunContext is passed to each node Func. Store lets nodes persist artifacts
// keyed by the node's idempotency key so resumed runs reuse prior outputs.
type RunContext struct {
	store  *Store
	rec    *core.Recorder
	nodeID string
	key    string
}

// SaveArtifact records a node output under its idempotency key.
func (c *RunContext) SaveArtifact(data []byte) error {
	return c.store.put(c.key, data)
}

// LoadArtifact returns a previously saved output, enabling resume/reuse.
func (c *RunContext) LoadArtifact() ([]byte, bool) {
	return c.store.get(c.key)
}

// Recorder exposes the shared fault sink to node implementations.
func (c *RunContext) Recorder() *core.Recorder { return c.rec }

// nodeState is the mutable runtime record of one node.
type nodeState struct {
	status Status
	tries  int
	key    string
	err    error
}

// Result summarizes the whole run.
type Result struct {
	Statuses     map[string]Status `json:"statuses"`
	FirstBlocked string            `json:"first_blocked,omitempty"`
	RetriedReused int              `json:"retry_reused"`
	Failed       []string          `json:"failed,omitempty"`
}

// Executor runs a validated DAG.
type Executor struct {
	cfg   Config
	store *Store
	rec   *core.Recorder
}

// NewExecutor builds an executor. The store may be pre-populated to resume.
func NewExecutor(cfg Config, store *Store, rec *core.Recorder) *Executor {
	if cfg.Workers <= 0 {
		cfg.Workers = 1
	}
	if cfg.MaxRetry < 0 {
		cfg.MaxRetry = 0
	}
	return &Executor{cfg: cfg, store: store, rec: rec}
}

// Run executes the graph with bounded worker concurrency. A node starts only
// after all dependencies reach a terminal state; any failed dependency makes
// it blocked. FirstBlocked is the earliest blocked node in topological order.
func (e *Executor) Run(d *DAG) (*Result, error) {
	states := map[string]*nodeState{}
	for _, id := range d.order {
		states[id] = &nodeState{status: StatusPending, key: d.nodes[id].Key()}
	}
	var mu sync.Mutex
	cond := sync.NewCond(&mu)
	completed := map[string]bool{}
	sem := make(chan struct{}, e.cfg.Workers)
	reused := 0

	canStart := func(id string) (bool, bool) {
		n := d.nodes[id]
		blockedByFailure := false
		for _, dep := range n.Deps {
			switch states[dep].status {
			case StatusFailed, StatusBlocked:
				blockedByFailure = true
			case StatusSuccess:
			default:
				return false, false // dependency still running/pending
			}
		}
		return true, blockedByFailure
	}

	var wg sync.WaitGroup
	for {
		mu.Lock()
		progressed := false
		for _, id := range d.order {
			st := states[id]
			if st.status != StatusPending || completed[id] {
				continue
			}
			ready, blocked := canStart(id)
			if !ready {
				continue
			}
			progressed = true
			completed[id] = true
			if blocked {
				st.status = StatusBlocked
				e.rec.Add(core.NewFault(core.FaultBlocked, core.SevBlocking,
					"upstream dependency failed",
					core.Source{Segment: id}))
				cond.Broadcast()
				continue
			}
			st.status = StatusRunning
			wg.Add(1)
			go func(nid string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				err := e.runNode(d.nodes[nid], states[nid], &reused)
				mu.Lock()
				if err != nil {
					states[nid].status = StatusFailed
					states[nid].err = err
				} else {
					states[nid].status = StatusSuccess
				}
				mu.Unlock()
				cond.Broadcast()
			}(id)
		}
		// Termination: no pending work and every node terminal.
		allTerminal := true
		for _, id := range d.order {
			if states[id].status == StatusPending || states[id].status == StatusRunning {
				allTerminal = false
			}
		}
		if allTerminal {
			mu.Unlock()
			break
		}
		if !progressed {
			// Wait for an in-flight node to finish before re-scanning.
			cond.Wait()
		}
		mu.Unlock()
	}
	wg.Wait()

	return e.buildResult(d, states, reused), nil
}

// runNode applies retries against the idempotency key. An artifact already in
// the store means the node (or a prior attempt) produced output, so it is
// reused instead of recomputed — the resume/idempotency guarantee.
func (e *Executor) runNode(n *Node, st *nodeState, reused *int) error {
	if _, ok := e.store.get(st.key); ok {
		*reused++
		return nil
	}
	var lastErr error
	for attempt := 0; attempt <= e.cfg.MaxRetry; attempt++ {
		st.tries = attempt + 1
		ctx := &RunContext{store: e.store, rec: e.rec, nodeID: n.ID, key: st.key}
		if err := n.Run(ctx); err != nil {
			lastErr = err
			e.rec.Add(core.NewFault(core.FaultRetryExhausted, core.SevWarn,
				fmt.Sprintf("node %s attempt %d failed: %v", n.ID, attempt+1, err),
				core.Source{Segment: n.ID}))
			continue
		}
		return nil
	}
	return lastErr
}

func (e *Executor) buildResult(d *DAG, states map[string]*nodeState, reused int) *Result {
	res := &Result{
		Statuses: map[string]Status{}, RetriedReused: reused,
	}
	for _, id := range d.order {
		res.Statuses[id] = states[id].status
		if states[id].status == StatusFailed {
			res.Failed = append(res.Failed, id)
		}
		if states[id].status == StatusBlocked && res.FirstBlocked == "" {
			res.FirstBlocked = id // topological order => first blocker
		}
	}
	return res
}

// ErrRetryExhausted is a convenience for callers that want a typed failure.
var ErrRetryExhausted = errors.New("schedule: retries exhausted")
