package app

import (
	"context"
	"time"

	"mediaproc/internal/model"
	"mediaproc/internal/pipeline"
)

// RunQueueCancelDemo deterministically fills a bounded queue, cancels the
// blocked producer, and reports the original blocked item offset.
func RunQueueCancelDemo(opts Options) (*Report, error) {
	q := opts.Queue
	if q <= 0 {
		q = 4
	}
	mem := opts.MaxMemory
	if mem <= 0 {
		mem = 8 << 20
	}
	r := pipeline.NewRunner(q, mem, 0)
	cleaned := make(chan int64, q+1)
	r.Cleanup = func(item pipeline.Item) { cleaned <- item.Offset }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	releaseConsumer := make(chan struct{})
	runErr := make(chan error, 1)
	go func() {
		runErr <- r.Run(ctx, func(callCtx context.Context, _ pipeline.Item) error {
			close(started)
			select {
			case <-releaseConsumer:
				return nil
			case <-callCtx.Done():
				return callCtx.Err()
			}
		})
	}()

	if err := r.Submit(context.Background(), pipeline.Item{Offset: 1000, Size: 1}); err != nil {
		return nil, err
	}
	<-started
	for i := 0; i < q; i++ {
		if err := r.Submit(context.Background(), pipeline.Item{
			Offset: int64(2000 + i),
			Size:   1,
		}); err != nil {
			return nil, err
		}
	}

	const blockedOffset int64 = 9001
	submitErr := make(chan error, 1)
	go func() {
		submitErr <- r.Submit(ctx, pipeline.Item{Offset: blockedOffset, Size: 1})
	}()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && r.Metrics().PeakQueue < q {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(10 * time.Millisecond) // let the final producer block on the full queue
	cancel()

	serr := <-submitErr
	if fe, ok := model.AsError(serr); !ok || fe.Offset != blockedOffset {
		close(releaseConsumer)
		return nil, model.InvariantError(model.PhasePipeline, blockedOffset,
			"queue cancellation did not preserve blocked offset", serr)
	}
	<-runErr
	close(releaseConsumer)

	cancelled := 0
	for cancelled < q+1 {
		select {
		case <-cleaned:
			cancelled++
		case <-time.After(time.Second):
			return nil, model.InvariantError(model.PhasePipeline, blockedOffset,
				"timed out waiting for unpublished cleanup", nil)
		}
	}

	m := r.Metrics()
	return &Report{
		Cancelled:        cancelled,
		FirstErrorOffset: blockedOffset,
		PeakQueue:        m.PeakQueue,
		PeakMemory:       m.PeakMemory,
		MemoryLimit:      mem,
		ExitCode:         0,
	}, nil
}
