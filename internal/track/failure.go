package track

import (
	"context"
	"fmt"

	"avpipe/internal/fault"
)

// Status is the outcome of one track's execution.
type Status string

const (
	StatusOK       Status = "ok"
	StatusFailed   Status = "failed"
	StatusCanceled Status = "canceled"
)

// Outcome records what happened to every node.
type Outcome struct {
	Order      []string
	Status     map[string]Status
	Failures   map[string]error
	FirstError error
}

// Failed lists failed nodes in execution order.
func (o *Outcome) Failed() []string { return o.withStatus(StatusFailed) }

// Canceled lists canceled nodes in execution order.
func (o *Outcome) Canceled() []string { return o.withStatus(StatusCanceled) }

func (o *Outcome) withStatus(want Status) []string {
	var out []string
	for _, name := range o.Order {
		if o.Status[name] == want {
			out = append(out, name)
		}
	}
	return out
}

// Run executes each node in topological order. When a node fails, every
// transitive dependent is marked canceled and skipped, so a bad audio block
// cannot let a derived video track pretend to succeed. The first root cause
// is preserved as Outcome.FirstError and is the cause of every cancellation.
func (g *Graph) Run(ctx context.Context, run func(ctx context.Context, name string) error) (*Outcome, error) {
	order, err := g.Compile()
	if err != nil {
		return nil, err
	}
	out := &Outcome{
		Order:    order,
		Status:   make(map[string]Status, len(order)),
		Failures: make(map[string]error),
	}
	canceled := make(map[string]error)
	var first error
	for _, name := range order {
		if cause, ok := canceled[name]; ok {
			out.Status[name] = StatusCanceled
			out.Failures[name] = cause
			continue
		}
		if err := ctx.Err(); err != nil {
			root := fault.Wrap(fault.CodeQueueCanceled, "track.run", err,
				"execution canceled before %q", name).WithTrack(name)
			out.Status[name] = StatusCanceled
			out.Failures[name] = root
			if first == nil {
				first = root
			}
			g.markDownstream(name, root, canceled)
			continue
		}
		if err := run(ctx, name); err != nil {
			root := fault.Root(err)
			if root.Track == "" {
				root.Track = name
			}
			out.Status[name] = StatusFailed
			out.Failures[name] = root
			if first == nil {
				first = root
			}
			g.markDownstream(name, root, canceled)
			continue
		}
		out.Status[name] = StatusOK
	}
	out.FirstError = first
	return out, first
}

// markDownstream records a cancellation for every transitive dependent.
func (g *Graph) markDownstream(failed string, cause error, canceled map[string]error) {
	queue := []string{failed}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, dep := range g.DependentsOf(cur) {
			if _, done := canceled[dep]; done {
				continue
			}
			canceled[dep] = fault.Wrap(fault.CodeTrackFailed, "track.run", cause,
				"canceled: dependency %q failed", cur).WithTrack(dep)
			queue = append(queue, dep)
		}
	}
}

// Summary renders one deterministic line describing the outcome.
func (o *Outcome) Summary() string {
	s := fmt.Sprintf("tracks=%d failed=%d canceled=%d",
		len(o.Order), len(o.Failed()), len(o.Canceled()))
	if o.FirstError != nil {
		s += " first=" + fault.Root(o.FirstError).Error()
	}
	return s
}
