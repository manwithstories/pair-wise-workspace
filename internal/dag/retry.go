package dag

import (
	"fmt"

	"mediapack/internal/fault"
)

// RetryPolicy bounds automatic retries of a node whose failure is transient.
// Only failures classified as internal are retried: input, limit, usage and
// dependency failures are deterministic and would fail identically.
type RetryPolicy struct {
	MaxAttempts int
}

// DefaultRetry is two attempts total: one retry.
func DefaultRetry() RetryPolicy { return RetryPolicy{MaxAttempts: 2} }

// WithRetry wraps a node's Run so that a transient failure is retried while
// the idempotency key stays unchanged.  The node's output hash is therefore
// identical whether it succeeded on the first or the last attempt, which is
// what makes a resumed run comparable with an uninterrupted one.
func WithRetry(n Node, policy RetryPolicy) Node {
	if policy.MaxAttempts < 1 {
		policy.MaxAttempts = 1
	}
	original := n.Run
	n.Run = func() (string, error) {
		var last error
		for attempt := 1; attempt <= policy.MaxAttempts; attempt++ {
			out, err := original()
			if err == nil {
				return out, nil
			}
			last = err
			if fault.ClassOf(err) != fault.ClassInternal {
				break
			}
		}
		return "", last
	}
	return n
}

// BlockedChain walks the state from a blocked or failed node back to the
// earliest unfinished dependency and returns the chain in execution order.
// It is what the terminal prints as first_blocked; callers never have to
// guess which input to regenerate.
func BlockedChain(state *State, start string) ([]string, error) {
	if state == nil {
		return nil, fault.New(fault.ClassInternal, "dag", start, "nil-state", "nil state")
	}
	rec, ok := state.Nodes[start]
	if !ok {
		return nil, fault.New(fault.ClassInput, "dag", start, "unknown-node",
			fmt.Sprintf("node %q has no state record", start))
	}
	chain := []string{start}
	seen := map[string]bool{start: true}
	for rec.Blocked != "" {
		chain = append(chain, rec.Blocked)
		if seen[rec.Blocked] {
			return nil, fault.New(fault.ClassInternal, "dag", start, "blocked-cycle",
				"blocked chain repeats a node")
		}
		seen[rec.Blocked] = true
		next, ok := state.Nodes[rec.Blocked]
		if !ok {
			break
		}
		rec = next
	}
	// Reverse so the earliest blocker is first.
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain, nil
}

// EarliestBlocked returns the smallest first-blocker name across every
// blocked or failed record, compared lexicographically after topological
// position is lost.  The caller passes the execution order to break ties.
func EarliestBlocked(state *State, order []string) *fault.Error {
	if state == nil {
		return nil
	}
	for _, name := range order {
		rec, ok := state.Nodes[name]
		if !ok || (rec.Status != StatusBlocked && rec.Status != StatusFailed) {
			continue
		}
		blockedBy := rec.Blocked
		if blockedBy == "" {
			blockedBy = name
		}
		return fault.Blocked("dag", name, "dag:"+blockedBy, 0, 0)
	}
	return nil
}

// AllDone reports whether every named node reached StatusDone.
func AllDone(state *State, names []string) bool {
	if state == nil {
		return false
	}
	for _, name := range names {
		rec, ok := state.Nodes[name]
		if !ok || rec.Status != StatusDone {
			return false
		}
	}
	return true
}
