// Package pipeline executes transcode jobs. A worker pool runs independent
// jobs concurrently, while GOPs inside one job remain strictly serial.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"turncode/internal/config"
	"turncode/internal/state"
)

var (
	ErrInterrupted  = errors.New("interrupted at GOP boundary")
	ErrInvalidJob   = errors.New("invalid job")
	ErrWorkConflict = errors.New("work directory already contains a job")
)

type Job struct {
	Spec           config.Spec
	SourcePath     string
	WorkDir        string
	QPOffset       int
	FailAfterGOP   int
	CancelAfterGOP int
	Resume         bool
}

type InterruptedError struct {
	CompletedGOPs int
	Cancelled     bool
}

func (e *InterruptedError) Error() string {
	if e.Cancelled {
		return fmt.Sprintf("cancelled at GOP boundary after %d GOPs", e.CompletedGOPs)
	}
	return fmt.Sprintf("interrupted at GOP boundary after %d GOPs", e.CompletedGOPs)
}

func (e *InterruptedError) Unwrap() error { return ErrInterrupted }

type Outcome struct {
	Completed      bool
	Interrupted    bool
	Cancelled      bool
	CompletedGOPs  int
	ResumedFromGOP int
	BitstreamBytes int64
	Checkpoint     state.Checkpoint
}

type Result struct {
	Job     *Job
	Outcome Outcome
	Err     error
}

type Scheduler struct {
	workers int
}

func NewScheduler(workers int) *Scheduler {
	if workers < 1 {
		workers = 1
	}
	if workers > 16 {
		workers = 16
	}
	return &Scheduler{workers: workers}
}

// Run executes all jobs and preserves submission order in its results.
func (s *Scheduler) Run(ctx context.Context, jobs []*Job) []Result {
	results := make([]Result, len(jobs))
	if len(jobs) == 0 {
		return results
	}
	indexes := make(chan int)
	var wg sync.WaitGroup
	workers := s.workers
	if workers > len(jobs) {
		workers = len(jobs)
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range indexes {
				job := jobs[idx]
				out, err := RunJob(ctx, job)
				results[idx] = Result{Job: job, Outcome: out, Err: err}
			}
		}()
	}
	for i := range jobs {
		select {
		case indexes <- i:
		case <-ctx.Done():
			close(indexes)
			wg.Wait()
			for j := i; j < len(jobs); j++ {
				results[j] = Result{Job: jobs[j], Err: ctx.Err()}
			}
			return results
		}
	}
	close(indexes)
	wg.Wait()
	return results
}
