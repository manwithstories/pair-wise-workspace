package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
)

// maxRetries bounds retry attempts for reentrant nodes.
const maxRetries = 2

// idempotencyKey derives the node's idempotency key from its input
// digests (dependency keys), parameters and node version. Equal keys mean
// equal outputs, so reruns neither re-execute nor duplicate side effects.
func idempotencyKey(n *Node, depKeys []string) string {
	h := sha256.New()
	h.Write([]byte(n.Name))
	h.Write([]byte{0})
	h.Write([]byte(n.Version))
	h.Write([]byte{0})
	if n.Params != nil {
		h.Write([]byte(n.Params()))
	}
	for _, k := range depKeys {
		h.Write([]byte{0})
		h.Write([]byte(k))
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// exec runs one node with idempotent memoization and bounded retry. Only
// reentrant nodes are retried; a non-reentrant node fails on first error.
func (s *Scheduler) exec(ctx context.Context, n *Node, deps map[string]any,
	depKeys []string) (any, error) {
	key := idempotencyKey(n, depKeys)
	s.keys[n.Name] = key
	if s.done[key] {
		return s.results[n.Name], nil // already validated: reuse
	}
	var err error
	var out any
	for attempt := 0; ; attempt++ {
		out, err = n.Run(ctx, deps)
		if err == nil {
			s.done[key] = true
			s.results[n.Name] = out
			return out, nil
		}
		if !n.Reentrant || attempt >= maxRetries {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
}

// Key returns the idempotency key computed for a node, for reporting.
func (s *Scheduler) Key(name string) string { return s.keys[name] }

// DoneKeys lists completed idempotency keys (sorted by node insertion).
func (s *Scheduler) DoneKeys() []string {
	var out []string
	for _, name := range s.order {
		if s.done[s.keys[name]] {
			out = append(out, name+":"+s.keys[name])
		}
	}
	return out
}
