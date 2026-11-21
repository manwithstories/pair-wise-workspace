// Package pipeline schedules processing stages in track-graph
// dependency order and stops output commits on the first failure.
package pipeline

import (
	"mediaroute/internal/model"
)

// Context carries shared state between stages.
type Context struct {
	Values map[string]any
}

func NewContext() *Context { return &Context{Values: map[string]any{}} }

// Stage is one named processing node with its dependencies.
type Stage struct {
	Node model.Node
	Run  func(ctx *Context) error
}

// Pipeline executes stages in topological order.
type Pipeline struct {
	order  []string
	stages map[string]Stage
}

// New validates the track graph (missing deps, cycles) and returns a
// ready pipeline. Graph errors name the offending node.
func New(stages ...Stage) (*Pipeline, error) {
	g := &model.Graph{}
	for _, s := range stages {
		g.Nodes = append(g.Nodes, s.Node)
	}
	order, err := g.TopoSort()
	if err != nil {
		return nil, err
	}
	m := map[string]Stage{}
	for _, s := range stages {
		m[s.Node.ID] = s
	}
	return &Pipeline{order: order, stages: m}, nil
}

// Order returns the execution order of stage IDs.
func (p *Pipeline) Order() []string { return p.order }

// Run executes stages in dependency order. The first error stops the
// run: downstream stages never execute, so no further output commits.
// MediaError context is preserved as it propagates up the stage chain.
func (p *Pipeline) Run(ctx *Context) error {
	for _, id := range p.order {
		s := p.stages[id]
		if err := s.Run(ctx); err != nil {
			if me, ok := err.(*model.MediaError); ok {
				return me
			}
			return &model.MediaError{Code: model.ErrInternal, Module: "pipeline",
				Msg: "stage " + id + " failed", Cause: err}
		}
	}
	return nil
}
