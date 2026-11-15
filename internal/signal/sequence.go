package signal

import "sort"

type sequenceState struct {
	next    uint64
	started bool
	pending map[uint64]Envelope
}

type SequenceCache struct {
	sources map[string]*sequenceState
}

func NewSequenceCache() *SequenceCache {
	return &SequenceCache{sources: make(map[string]*sequenceState)}
}

// Accept returns ordered envelopes ready for delivery. Future sequence numbers
// are retained until the missing gap arrives; duplicate and stale messages are
// ignored. The first seen sequence establishes the source baseline.
func (c *SequenceCache) Accept(env Envelope) ([]Envelope, bool) {
	if c.sources == nil {
		c.sources = make(map[string]*sequenceState)
	}
	state, ok := c.sources[env.Source]
	if !ok {
		state = &sequenceState{next: env.Seq, started: true, pending: make(map[uint64]Envelope)}
		c.sources[env.Source] = state
	}
	if env.Seq < state.next || (env.Seq == state.next-1 && state.next > 0) {
		return nil, false
	}
	if _, exists := state.pending[env.Seq]; exists {
		return nil, false
	}
	state.pending[env.Seq] = env
	ready := make([]Envelope, 0, len(state.pending))
	for {
		current, exists := state.pending[state.next]
		if !exists {
			break
		}
		delete(state.pending, state.next)
		ready = append(ready, current)
		state.next++
	}
	if len(ready) == 0 {
		return nil, true
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i].Seq < ready[j].Seq })
	return ready, true
}

func (c *SequenceCache) Pending() int {
	total := 0
	for _, state := range c.sources {
		total += len(state.pending)
	}
	return total
}
