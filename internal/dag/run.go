package dag

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"medpipe/internal/model"
)

// Run executes the graph. Every ready node is attempted; a node whose
// dependency failed is marked blocked and its first blocking dependency in
// topological order is reported on the run result.
func (s *Scheduler) Run() (*RunResult, error) {
	results := make(map[string]Result, len(s.nodes))
	rr := &RunResult{Results: results}
	pos := make(map[string]int, len(s.order))
	for i, n := range s.order {
		pos[n] = i
	}
	for _, name := range s.order {
		node := s.nodes[name]
		var inputs InputDigests
		var blockedBy string
		for _, dep := range node.Deps {
			r, ok := results[dep]
			if !ok {
				// Cannot happen in a valid topological order.
				blockedBy = dep
				break
			}
			if r.State != StateDone && r.State != StateReused {
				// Keep the dependency that appears earliest in topological
				// order, which is the one a human should fix first.
				if blockedBy == "" || pos[dep] < pos[blockedBy] {
					blockedBy = dep
				}
				continue
			}
			inputs = append(inputs, hex.EncodeToString(r.Digest[:]))
		}
		if blockedBy != "" {
			results[name] = Result{
				Name:   name,
				State:  StateBlocked,
				Detail: fmt.Sprintf("blocked by %s", blockedBy),
				Error:  fmt.Sprintf("dependency %s did not complete", blockedBy),
			}
			rr.Blocked = append(rr.Blocked, BlockedDep{Node: name, Dep: blockedBy})
			continue
		}
		key, err := Key(name, inputs, node.Params)
		if err != nil {
			return nil, err
		}
		res, err := s.executeOnce(name, key, inputs)
		if err != nil {
			return nil, err
		}
		results[name] = res
		switch res.State {
		case StateReused:
			rr.ReusedNodes++
		case StateDone:
			rr.ExecutedNodes++
		case StateFailed:
			rr.FailedNodes = append(rr.FailedNodes, name)
		}
	}
	rr.Results = results
	rr.Runs = make(map[string]int, len(s.nodes))
	s.mu.Lock()
	for name := range s.nodes {
		rr.Runs[name] = s.runs[name]
	}
	s.mu.Unlock()
	// Report the first blocked node in topological order.
	if len(rr.Blocked) > 0 {
		byNode := make(map[string]BlockedDep, len(rr.Blocked))
		for _, bd := range rr.Blocked {
			if _, ok := byNode[bd.Node]; !ok {
				byNode[bd.Node] = bd
			}
		}
		for _, name := range s.order {
			if bd, ok := byNode[name]; ok {
				first := bd
				rr.FirstBlocked = &first
				break
			}
		}
	}
	return rr, nil
}

// executeOnce is the single-flight execution path: concurrent callers with
// the same node+key share one Run invocation and one cache write.
func (s *Scheduler) executeOnce(name, key string, inputs InputDigests) (Result, error) {
	s.mu.Lock()
	if wg, ok := s.inflight[key]; ok {
		s.mu.Unlock()
		wg.Wait()
		s.mu.Lock()
		r, ok := s.results[name]
		s.mu.Unlock()
		if !ok {
			return Result{}, model.New(model.KindInvariant, "dag.execute", "finished node has no recorded result")
		}
		return r, nil
	}
	wg := &sync.WaitGroup{}
	wg.Add(1)
	s.inflight[key] = wg
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.inflight, key)
		s.mu.Unlock()
		wg.Done()
	}()

	if rec, ok := s.cache.Lookup(key); ok {
		var d [32]byte
		raw, err := hex.DecodeString(rec.Digest)
		if err != nil || len(raw) != 32 {
			return Result{}, model.New(model.KindCorrupt, "dag.execute", fmt.Sprintf("stored digest for %s is malformed", name))
		}
		copy(d[:], raw)
		res := Result{Name: name, Key: key, State: StateReused, Digest: d, Detail: rec.Detail, Reused: true}
		s.mu.Lock()
		s.results[name] = res
		s.mu.Unlock()
		return res, nil
	}

	var lastErr error
	attempts := s.opt.MaxAttempts
	for attempt := 1; attempt <= attempts; attempt++ {
		sum, err := s.nodes[name].Run(key)
		s.mu.Lock()
		s.runs[key]++
		s.mu.Unlock()
		if err == nil {
			digest := sum.Digest
			if digest == ([32]byte{}) {
				digest = sha256.Sum256([]byte(name + "\x00" + key))
			}
			rec := Record{
				Key: key, Node: name, Inputs: inputStrings(inputs),
				Digest: hex.EncodeToString(digest[:]), Detail: sum.Detail,
			}
			if _, _, perr := s.cache.Put(rec); perr != nil {
				return Result{}, perr
			}
			res := Result{Name: name, Key: key, State: StateDone, Digest: digest, Detail: sum.Detail, Runs: attempt}
			s.mu.Lock()
			s.results[name] = res
			s.mu.Unlock()
			return res, nil
		}
		lastErr = err
		// A typed input/corruption failure is not retried blindly; retrying a
		// checksum failure just reproduces it.
		var me *model.Error
		if errors.As(err, &me) && (me.Kind == model.KindCorrupt || me.Kind == model.KindInput) {
			break
		}
	}
	res := Result{Name: name, Key: key, State: StateFailed, Error: lastErr.Error(), Runs: attempts}
	s.mu.Lock()
	s.results[name] = res
	s.mu.Unlock()
	return res, nil
}

// RunResult is the outcome of one full graph pass.
type RunResult struct {
	Results map[string]Result
	Blocked []BlockedDep
	// FirstBlocked is the first blocked node in topological order.
	FirstBlocked *BlockedDep
	// ReusedNodes counts nodes satisfied from the cache.
	ReusedNodes int
	// ExecutedNodes counts nodes that actually ran.
	ExecutedNodes int
	// FailedNodes lists failed node names in topological order.
	FailedNodes []string
	// Runs is how many times each node actually executed, keyed by node name.
	Runs map[string]int
}

// BlockedDep pairs a blocked node with the dependency that stopped it.
type BlockedDep struct {
	Node string
	Dep  string
}

func inputStrings(in InputDigests) []string {
	out := make([]string, len(in))
	copy(out, in)
	return out
}
