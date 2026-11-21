package dag

import (
	"fmt"
	"sync"

	"mediad/internal/model"
)

// Status is the lifecycle state of a node in one run.
type Status string

// Node lifecycle states.
const (
	StatusPending Status = "pending"
	StatusRunning Status = "running"
	StatusSuccess Status = "success"
	StatusFailed  Status = "failed"
	StatusBlocked Status = "blocked"
	StatusSkipped Status = "skipped" // reused from checkpoint, not re-run
)

// CheckpointStore persists completed node results across process restarts.
type CheckpointStore interface {
	// Load returns the stored key for id; empty string means no record.
	Load(id string) (key string, err error)
	// Save records a completed node keyed by its idempotent key.
	Save(id, key string) error
}

// RunContext is passed to every node function.
type RunContext struct {
	NodeID  string
	Key     string
	Attempt int
}

// Result captures what happened to one node.
type Result struct {
	ID          string `json:"id"`
	Key         string `json:"key"`
	Status      Status `json:"status"`
	Attempts    int    `json:"attempts"`
	Error       string `json:"error,omitempty"`
	Recoverable bool   `json:"recoverable,omitempty"`
}

// Options controls a run.
type Options struct {
	Concurrency int
	MaxAttempts int // >= 1
	// Interrupt, when set and returning true, makes the runner stop starting
	// new nodes (used by the resume demo); started nodes finish cleanly.
	Interrupt func(completed int) bool
}

// Runner executes a graph against a checkpoint store.
type Runner struct {
	graph *Graph
	store CheckpointStore
}

// NewRunner wires a validated graph with its durable store.
func NewRunner(g *Graph, store CheckpointStore) *Runner {
	return &Runner{graph: g, store: store}
}

// Run executes every node. Already-checkpointed nodes whose idempotent key
// matches are skipped; a changed key invalidates and re-runs the node and
// every dependent. The returned summary is ordered topologically.
func (r *Runner) Run(opts Options) ([]Result, error) {
	if opts.Concurrency <= 0 {
		opts.Concurrency = 1
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 1
	}
	keys, err := r.graph.AllKeys()
	if err != nil {
		return nil, err
	}
	results := make(map[string]*Result, len(r.graph.order))
	status := make(map[string]Status, len(r.graph.order))
	for _, id := range r.graph.order {
		results[id] = &Result{ID: id, Key: keys[id].Value, Status: StatusPending}
		status[id] = StatusPending
	}
	var (
		mu        sync.Mutex
		wg        sync.WaitGroup
		sem       = make(chan struct{}, opts.Concurrency)
		completed int
		stopped   bool
		cond      = sync.NewCond(&mu)
	)
	canStart := func(id string) bool {
		if status[id] != StatusPending {
			return false
		}
		for _, in := range r.graph.nodes[id].Inputs {
			if status[in] != StatusSuccess && status[in] != StatusSkipped {
				return false
			}
		}
		return true
	}
	for {
		mu.Lock()
		var id string
		for _, cand := range r.graph.order {
			if canStart(cand) {
				id = cand
				break
			}
		}
		if id == "" {
			// Nothing startable: either everything settled, or the run was
			// interrupted with pending nodes left behind.
			allSettled := true
			for _, st := range status {
				if st == StatusPending || st == StatusRunning {
					allSettled = false
				}
			}
			if allSettled {
				mu.Unlock()
				break
			}
			cond.Wait()
			mu.Unlock()
			continue
		}
		if !stopped && opts.Interrupt != nil && opts.Interrupt(completed) {
			stopped = true
		}
		if stopped {
			// Mark remaining pending work blocked so callers see the
			// topological first blocker and can resume later.
			for pid, st := range status {
				if st == StatusPending {
					status[pid] = StatusBlocked
					results[pid].Status = StatusBlocked
					results[pid].Error = model.ErrBlocked.Error()
					results[pid].Recoverable = true
				}
			}
			mu.Unlock()
			break
		}
		status[id] = StatusRunning
		results[id].Status = StatusRunning
		wg.Add(1)
		sem <- struct{}{}
		go func(nodeID string) {
			defer wg.Done()
			defer func() { <-sem }()
			r.execute(nodeID, keys[nodeID].Value, opts, results, status, &mu, cond, &completed)
		}(id)
		mu.Unlock()
	}
	wg.Wait()
	out := make([]Result, 0, len(r.graph.order))
	for _, id := range r.graph.order {
		out = append(out, *results[id])
	}
	return out, nil
}

func (r *Runner) execute(id, key string, opts Options, results map[string]*Result,
	status map[string]Status, mu *sync.Mutex, cond *sync.Cond, completed *int) {
	// Checkpoint short-circuit: equal key means the stored output is valid.
	if stored, err := r.store.Load(id); err == nil && stored == key {
		mu.Lock()
		status[id] = StatusSkipped
		res := results[id]
		res.Status, res.Attempts = StatusSkipped, 0
		*completed++
		cond.Broadcast()
		mu.Unlock()
		return
	}
	node := r.graph.nodes[id]
	var lastErr error
	recoverable := false
	attempts := 0
	for attempt := 1; attempt <= opts.MaxAttempts; attempt++ {
		attempts = attempt
		ctx := &RunContext{NodeID: id, Key: key, Attempt: attempt}
		if err := node.Run(ctx); err != nil {
			lastErr = err
			if me, ok := err.(*model.MediaError); ok {
				recoverable = me.Recoverable
			}
			continue
		}
		lastErr = nil
		break
	}
	mu.Lock()
	defer mu.Unlock()
	res := results[id]
	res.Attempts = attempts
	if lastErr != nil {
		status[id] = StatusFailed
		res.Status, res.Error = StatusFailed, lastErr.Error()
		res.Recoverable = recoverable
		// Propagate in topological order: every still-pending dependent is
		// blocked; the first one overall is what reports surface.
		for _, dep := range r.graph.order {
			if status[dep] == StatusPending {
				if blk, _ := r.graph.FirstBlocker(status, dep); blk != "" {
					status[dep] = StatusBlocked
					results[dep].Status = StatusBlocked
					results[dep].Error = fmt.Sprintf("%s: upstream %s", model.ErrBlocked, blk)
					results[dep].Recoverable = true
				}
			}
		}
	} else {
		if err := r.store.Save(id, key); err != nil {
			status[id] = StatusFailed
			res.Status, res.Error = StatusFailed, err.Error()
		} else {
			status[id] = StatusSuccess
			res.Status = StatusSuccess
		}
	}
	*completed++
	cond.Broadcast()
}

// FirstBlocked returns the first node in topological order that is blocked or
// failed, plus its status. Empty id means the whole graph completed.
func FirstBlocked(results []Result) (string, Status) {
	for _, res := range results {
		if res.Status == StatusFailed || res.Status == StatusBlocked {
			return res.ID, res.Status
		}
	}
	return "", StatusSuccess
}
