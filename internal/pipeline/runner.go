// Package pipeline schedules nodes in dependency order, persists each
// node's result as an atomic checkpoint and recovers from corrupt
// state by rolling back to the last valid committed node.
package pipeline

import (
	"encoding/json"
	"fmt"

	"mediacore/internal/clock"
	"mediacore/internal/provenance"
	"mediacore/internal/store"
	"mediacore/internal/timeline"
)

// Context carries shared state through the run.
type Context struct {
	Store        *store.Store
	Prov         *provenance.Log
	InputDigest  string
	ParamsDigest string
	Data         map[string]any
	ErrorChain   []string
}

// Get returns a dependency's payload.
func (c *Context) Get(node string) any { return c.Data[node] }

// Node is one stage of the pipeline.
type Node struct {
	Name    string
	Deps    []string
	Version string
	// New returns a zero payload value used to decode checkpoints.
	New func() any
	// Run produces the node payload; it must be deterministic.
	Run func(ctx *Context) (any, error)
	// Prov optionally builds the provenance record for the output.
	Prov func(ctx *Context, out any) provenance.Record
}

// Summary describes one run.
type Summary struct {
	Commits                int
	Reused                 int
	DuplicateCommits       int
	RecoveredNodes         int
	RecoveryPosition       string
	FirstMissingDependency string
}

// Runner executes nodes in topological order.
type Runner struct {
	nodes   map[string]Node
	order   []string
	parents map[string][]string // node -> its dependencies
}

// New validates the graph and returns a ready runner.
func New(nodes []Node) (*Runner, error) {
	r := &Runner{nodes: make(map[string]Node), parents: make(map[string][]string)}
	tn := make([]timeline.Node, 0, len(nodes))
	for _, n := range nodes {
		if n.New == nil || n.Run == nil {
			return nil, fmt.Errorf("pipeline: node %q needs New and Run", n.Name)
		}
		r.nodes[n.Name] = n
		r.parents[n.Name] = n.Deps
		tn = append(tn, timeline.Node{Name: n.Name, Deps: n.Deps})
	}
	order, err := timeline.TopoSort(tn)
	if err != nil {
		return nil, err
	}
	r.order = order
	return r, nil
}

// Run executes all nodes, reusing valid checkpoints whose idempotency
// key matches, and recovering from corrupt checkpoints by rolling back
// the affected subgraph.
func (r *Runner) Run(ctx *Context) (*Summary, error) {
	sum := &Summary{}
	if ctx.Data == nil {
		ctx.Data = map[string]any{}
	}
	recovered := map[string]bool{}
	for _, name := range r.order {
		n := r.nodes[name]
		for _, d := range n.Deps {
			if _, ok := ctx.Data[d]; !ok {
				if sum.FirstMissingDependency == "" {
					sum.FirstMissingDependency = d
				}
				return sum, clock.At(clock.CodeDependencyMissing, "pipeline", "", -1,
					"node %s misses dependency %s", name, d)
			}
		}
		key := store.Key{
			Node:         name,
			InputDigest:  ctx.InputDigest,
			ParamsDigest: ctx.ParamsDigest,
			Version:      n.Version,
		}.IdempotencyKey()
		cp, err := ctx.Store.Load(name)
		if err != nil {
			if clock.CodeOf(err) != clock.CodeCheckpointCorrupt {
				return sum, err
			}
			// Corrupt checkpoint: roll back this node and everything
			// that depends on it, then re-execute from here.
			ctx.ErrorChain = append(ctx.ErrorChain, err.Error())
			if sum.FirstMissingDependency == "" {
				sum.FirstMissingDependency = name
			}
			if sum.RecoveryPosition == "" {
				sum.RecoveryPosition = name
			}
			r.rollback(ctx, name, recovered)
			cp = nil
		}
		if cp != nil && cp.Key == key {
			out := n.New()
			if err := json.Unmarshal(cp.Payload, out); err != nil {
				return sum, clock.At(clock.CodeCheckpointCorrupt, "pipeline", "", -1,
					"node %s payload: %v", name, err)
			}
			ctx.Data[name] = out
			sum.Reused++
			continue
		}
		out, err := n.Run(ctx)
		if err != nil {
			return sum, fmt.Errorf("pipeline: node %s: %w", name, err)
		}
		payload, err := json.Marshal(out)
		if err != nil {
			return sum, clock.Wrap(clock.CodeIO, "pipeline", err)
		}
		written, err := ctx.Store.Commit(name, key, payload)
		if err != nil {
			return sum, err
		}
		if written {
			sum.Commits++
			if ctx.Prov != nil && n.Prov != nil {
				if err := ctx.Prov.Append(n.Prov(ctx, out)); err != nil {
					return sum, err
				}
			}
		} else {
			sum.DuplicateCommits++
		}
		if recovered[name] {
			sum.RecoveredNodes++
		}
		ctx.Data[name] = out
	}
	return sum, nil
}

// rollback deletes the checkpoints of node and its transitive
// dependents so they are re-executed from validated inputs.
func (r *Runner) rollback(ctx *Context, node string, recovered map[string]bool) {
	affected := map[string]bool{node: true}
	for _, name := range r.order {
		if affected[name] {
			continue
		}
		for _, d := range r.parents[name] {
			if affected[d] {
				affected[name] = true
				break
			}
		}
	}
	for name := range affected {
		_ = ctx.Store.Remove(name)
		delete(ctx.Data, name)
		recovered[name] = true
	}
}
