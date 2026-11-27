package stream

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"sync"
	"time"

	"avpipe/internal/media"
	"avpipe/internal/timeline"
)

// SourceItem is one unit read from an input. DroppedBytes is set when the
// source had to discard an incomplete tail; the runner reports it.
type SourceItem struct {
	Block        media.Block
	Partial      bool
	DroppedBytes int64
	RawLen       int64
}

// Source streams input blocks. Read returns io.EOF once the input ends.
type Source interface {
	Read(strict bool) (SourceItem, error)
}

// Processor transforms one input block into zero or more final blocks.
type Processor interface {
	Process(in media.Block) ([]media.Block, error)
	Flush() ([]media.Block, error)
}

// Sink consumes final blocks. The demo sink is a slow consumer, which is what
// makes the bounded queue and its backpressure observable.
type Sink interface {
	Consume(block media.Block) error
}

// RunnerOptions configures the scheduler.
type RunnerOptions struct {
	Capacity     int
	MaxGapFrames int64
	MaxBlock     int
	Track        string
	Strict       bool
	Timeline     *timeline.Timeline
}

// Result reports pipeline counters after a run.
type Result struct {
	Queue           Stats
	Produced        uint64
	Consumed        uint64
	Gaps            uint64
	GapFrames       int64
	Duplicates      uint64
	DroppedBytes    int64
	Truncated       bool
	FlushedBlocks   uint64
	Canceled        bool
	lastConsumedSeq uint64
}

// Runner connects source, processor and sink through a bounded queue.
type Runner struct {
	opts RunnerOptions
}

// NewRunner validates options.
func NewRunner(opts RunnerOptions) (*Runner, error) {
	if opts.Capacity <= 0 {
		return nil, media.NewError(media.PhaseConfig, media.ExitUsage, opts.Track, 0, false, "runner",
			fmt.Errorf("queue capacity must be positive"))
	}
	if opts.MaxGapFrames < 0 {
		return nil, media.NewError(media.PhaseConfig, media.ExitUsage, opts.Track, 0, false, "runner",
			fmt.Errorf("max gap frames must be non-negative"))
	}
	if opts.MaxBlock <= 0 {
		opts.MaxBlock = 4096
	}
	if opts.Timeline == nil {
		return nil, media.NewError(media.PhaseConfig, media.ExitUsage, opts.Track, 0, false, "runner",
			fmt.Errorf("runner requires a timeline"))
	}
	return &Runner{opts: opts}, nil
}

// Run processes the source until EOF or cancellation. On cancellation the
// producer unblocks, the queue is closed, and the consumer still drains every
// block that was already enqueued, so the last complete block is preserved.
func (r *Runner) Run(ctx context.Context, src Source, proc Processor, sink Sink) (Result, error) {
	q, err := NewQueue[media.Block](r.opts.Capacity)
	if err != nil {
		return Result{}, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	res := Result{}
	var resMu sync.Mutex
	var prodErr, consErr error
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer q.Close()
		err := r.produce(runCtx, q, src, proc, &res, &resMu)
		if err != nil {
			prodErr = err
			cancel()
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		err := r.consume(ctx, q, sink, &res, &resMu)
		if err != nil {
			consErr = err
		}
	}()

	wg.Wait()
	resMu.Lock()
	res.Queue = q.Stats()
	resMu.Unlock()

	if ctx.Err() != nil {
		res.Canceled = true
		return res, media.NewError(media.PhaseIO, media.ExitInternal, r.opts.Track, 0, false, "run",
			fmt.Errorf("%w: %v", media.ErrCancelled, ctx.Err()))
	}
	if prodErr != nil {
		return res, prodErr
	}
	if consErr != nil {
		return res, consErr
	}
	return res, nil
}

func (r *Runner) produce(ctx context.Context, q *Queue[media.Block], src Source, proc Processor,
	res *Result, mu *sync.Mutex) error {
	track := r.opts.Track
	tl := r.opts.Timeline
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		item, err := src.Read(r.opts.Strict)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if item.DroppedBytes > 0 {
			mu.Lock()
			res.DroppedBytes += item.DroppedBytes
			res.Truncated = true
			mu.Unlock()
		}
		if item.Block.Frames() == 0 {
			continue
		}
		if err := item.Block.Validate(r.opts.MaxBlock); err != nil {
			return err
		}
		dup, err := tl.Accept(track, item.Block.Seq, hashBlock(item.Block))
		if err != nil {
			return err
		}
		if dup {
			mu.Lock()
			res.Duplicates++
			mu.Unlock()
			continue
		}

		lastEnd, started := tl.LastEnd(track)
		var gap int64
		if started && item.Block.Start > lastEnd {
			gap = item.Block.Start - lastEnd
		}
		if gap > r.opts.MaxGapFrames {
			return media.NewError(media.PhaseInput, media.ExitInput, track, item.Block.Seq, false, "gap",
				fmt.Errorf("gap of %d frames exceeds limit %d", gap, r.opts.MaxGapFrames))
		}
		if gap > 0 {
			if err := r.fillGap(ctx, q, proc, item.Block, lastEnd, gap, res, mu); err != nil {
				return err
			}
		}
		if _, err := tl.Advance(track, item.Block.Start, item.Block.Frames()); err != nil {
			return err
		}
		if err := r.forward(ctx, q, proc, item.Block, res, mu); err != nil {
			return err
		}
	}

	blocks, err := proc.Flush()
	if err != nil {
		return err
	}
	for _, b := range blocks {
		if err := r.push(ctx, q, b, res, mu); err != nil {
			return err
		}
		mu.Lock()
		res.FlushedBlocks++
		mu.Unlock()
	}
	return nil
}

// fillGap injects silence at the source rate so the processor sees a
// contiguous timeline. Gap length is bounded by MaxGapFrames.
func (r *Runner) fillGap(ctx context.Context, q *Queue[media.Block], proc Processor,
	next media.Block, from, gap int64, res *Result, mu *sync.Mutex) error {
	mu.Lock()
	res.Gaps++
	res.GapFrames += gap
	mu.Unlock()
	tl := r.opts.Timeline
	remaining := gap
	start := from
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := int64(r.opts.MaxBlock)
		if n > remaining {
			n = remaining
		}
		silence := media.Block{
			Track:    next.Track,
			Seq:      next.Seq,
			Start:    start,
			Rate:     next.Rate,
			Channels: next.Channels,
			Samples:  make([]float64, int(n)*next.Channels),
		}
		if _, err := tl.Advance(next.Track, start, int(n)); err != nil {
			return err
		}
		if err := r.forward(ctx, q, proc, silence, res, mu); err != nil {
			return err
		}
		start += n
		remaining -= n
	}
	return nil
}

func (r *Runner) forward(ctx context.Context, q *Queue[media.Block], proc Processor,
	in media.Block, res *Result, mu *sync.Mutex) error {
	out, err := proc.Process(in)
	if err != nil {
		return err
	}
	for _, b := range out {
		if err := r.push(ctx, q, b, res, mu); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) push(ctx context.Context, q *Queue[media.Block], b media.Block,
	res *Result, mu *sync.Mutex) error {
	if err := q.Push(ctx, b); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return err
	}
	mu.Lock()
	res.Produced++
	mu.Unlock()
	return nil
}

func (r *Runner) consume(ctx context.Context, q *Queue[media.Block], sink Sink,
	res *Result, mu *sync.Mutex) error {
	popCtx := ctx
	for {
		b, err := q.Pop(popCtx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				// Cancellation must not discard queued blocks: switch to a
				// draining context and keep consuming until the producer
				// closes the queue.
				popCtx = context.Background()
				continue
			}
			if errors.Is(err, ErrClosed) {
				return nil
			}
			return err
		}
		if err := sink.Consume(b); err != nil {
			return err
		}
		mu.Lock()
		res.Consumed++
		res.lastConsumedSeq = b.Seq
		mu.Unlock()
	}
}

// hashBlock is a cheap content fingerprint used for duplicate detection.
func hashBlock(b media.Block) uint64 {
	h := fnv.New64a()
	var buf [8]byte
	put := func(v uint64) {
		for i := 0; i < 8; i++ {
			buf[i] = byte(v >> (8 * i))
		}
		h.Write(buf[:])
	}
	put(uint64(b.Start))
	put(uint64(b.Channels))
	put(uint64(b.Rate.Num))
	put(uint64(b.Rate.Den))
	for _, s := range b.Samples {
		put(math.Float64bits(s))
	}
	return h.Sum64()
}

// StallSink delays every Consume by d to emulate a slow consumer. It is a
// wrapper so the demo can make backpressure deterministic without timing
// inside the pipeline itself.
type StallSink struct {
	Inner Sink
	Delay time.Duration
	Sleep func(time.Duration)
}

// Consume forwards the block after the configured delay.
func (s StallSink) Consume(b media.Block) error {
	if s.Delay > 0 {
		if s.Sleep != nil {
			s.Sleep(s.Delay)
		} else {
			time.Sleep(s.Delay)
		}
	}
	return s.Inner.Consume(b)
}
