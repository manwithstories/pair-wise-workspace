// Package sched runs segment jobs concurrently with failure injection.
package sched

import (
	"context"
	"errors"
	"sync"
)

// ErrInjected is returned when the injected failure point is reached.
var ErrInjected = errors.New("injected failure: stopping after requested segment count")

// Run executes fn for every index in pending with at most workers
// goroutines. onDone is invoked from the collector goroutine in
// completion order for each successful job. If failAfter >= 0, Run
// stops checkpointing after failAfter completions and returns
// ErrInjected; in-flight results are discarded so the persisted
// state matches exactly the reported completions.
func Run[T any](pending []int, workers, failAfter int, fn func(seg int) (T, error), onDone func(seg int, val T)) error {
	if workers < 1 {
		workers = 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type result struct {
		seg int
		val T
		err error
	}
	jobs := make(chan int)
	results := make(chan result, len(pending))
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for seg := range jobs {
				val, err := fn(seg)
				results <- result{seg: seg, val: val, err: err}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, seg := range pending {
			select {
			case jobs <- seg:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	completed := 0
	var firstErr error
	for r := range results {
		if r.err != nil && firstErr == nil {
			firstErr = r.err
			cancel()
		}
		if firstErr != nil {
			continue // drain remaining in-flight results
		}
		if failAfter >= 0 && completed >= failAfter {
			cancel()
			continue // discard in-flight results past the injection point
		}
		onDone(r.seg, r.val)
		completed++
	}
	if firstErr != nil {
		return firstErr
	}
	if failAfter >= 0 && completed >= failAfter && completed < len(pending) {
		return ErrInjected
	}
	return nil
}
