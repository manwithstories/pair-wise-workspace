package pipeline

import (
	"fmt"

	"relayctl/internal/dag"
	"relayctl/internal/transport"
)

// mediaNode is a concrete dag.Node whose behavior switches on its name. It
// closes over the Engine so every node shares one clock, journal and state.
type mediaNode struct {
	eng  *Engine
	name string
	deps []string
}

func (n *mediaNode) Name() string        { return n.name }
func (n *mediaNode) DependsOn() []string { return n.deps }

// InputDigests hashes the scenario identity; identical fixtures yield
// identical node keys, which is what makes resume idempotent.
func (n *mediaNode) InputDigests() map[string]string {
	return map[string]string{"scenario": n.eng.inputDigest()}
}

func (n *mediaNode) Parameters() map[string]string {
	return map[string]string{"case": n.eng.scenario.Manifest.Case, "seed":
		fmt.Sprintf("%d", n.eng.scenario.Manifest.Seed)}
}

func (n *mediaNode) Run(ctx *dag.Context) (string, error) {
	switch n.name {
	case NodeIngest:
		return n.eng.runIngest(ctx)
	case NodeRecover:
		return n.eng.runRecover(ctx)
	case NodeParse:
		return n.eng.runParse(ctx)
	case NodeSync:
		return n.eng.runSync(ctx)
	case NodePack:
		return n.eng.runPack(ctx)
	case NodeVerify:
		return n.eng.runVerify(ctx)
	default:
		return "", fmt.Errorf("pipeline: unknown node %q", n.name)
	}
}

// buildGraph assembles the relay dependency order.
func (e *Engine) buildGraph() []dag.Node {
	return []dag.Node{
		&mediaNode{eng: e, name: NodeIngest},
		&mediaNode{eng: e, name: NodeRecover, deps: []string{NodeIngest}},
		&mediaNode{eng: e, name: NodeParse, deps: []string{NodeRecover}},
		&mediaNode{eng: e, name: NodeSync, deps: []string{NodeParse}},
		&mediaNode{eng: e, name: NodePack, deps: []string{NodeSync}},
		&mediaNode{eng: e, name: NodeVerify, deps: []string{NodePack}},
	}
}

// runIngest loads packets into the bounded jitter buffer; ordering and
// duplicates come straight from the deterministic fixture plan.
func (e *Engine) runIngest(_ *dag.Context) (string, error) {
	jb := transport.NewJitterBuffer(transport.DefaultBufferCap)
	plan := e.scenario.Manifest.SequencePlan
	losses := map[uint16]bool{}
	for _, s := range plan.SingleLosses {
		losses[s] = true
	}
	for _, s := range plan.BurstLosses {
		losses[s] = true
	}
	accepted := 0
	for _, seq := range plan.Order {
		if losses[seq] {
			continue
		}
		if err := jb.Push(makePacket(seq)); err != nil {
			return "", err
		}
		accepted++
	}
	e.jitter = jb
	return fmt.Sprintf("ingested=%d duplicates=%d", accepted, jb.Stats().Duplicates), nil
}
