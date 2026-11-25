package pipeline

import (
	"context"
	"sort"
	"sync"

	"relay.local/relay/internal/fault"
	"relay.local/relay/internal/timeline"
)

// Merge collects per-job results and emits them in deterministic order
// regardless of completion order.
type Merge struct {
	mu      sync.Mutex
	entries map[int]timeline.Entry
	results map[int]any
	// SeqOf gives the stable sort key for a job ID; without it, job IDs sort
	// numerically after the timeline key.
	SeqOf func(entry timeline.Entry) [3]int64
	error *fault.List
}

// NewMerge creates an empty merger.
func NewMerge() *Merge {
	return &Merge{
		entries: map[int]timeline.Entry{},
		results: map[int]any{},
		error:   &fault.List{},
	}
}

// Add records the composed entry and the job's produced value.
func (m *Merge) Add(id int, entry timeline.Entry, value any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[id] = entry
	if value != nil {
		m.results[id] = value
	}
}

// AddError records a job failure without aborting the merge.
func (m *Merge) AddError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.error.Add(err)
}

// Len returns how many entries have been merged.
func (m *Merge) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries)
}

// Merged is one ordered output record.
type Merged struct {
	ID    int
	Entry timeline.Entry
	Value any
}

// Sorted returns every merged record in canonical order: start tick, track
// number, sample index, then job ID.
func (m *Merge) Sorted() []Merged {
	m.mu.Lock()
	ids := make([]int, 0, len(m.entries))
	out := make([]Merged, 0, len(m.entries))
	for id, e := range m.entries {
		ids = append(ids, id)
		out = append(out, Merged{ID: id, Entry: e, Value: m.results[id]})
	}
	m.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool {
		return lessEntry(out[i].Entry, out[j].Entry, out[i].ID, out[j].ID)
	})
	return out
}

func lessEntry(a, b timeline.Entry, ida, idb int) bool {
	if a.Start90k != b.Start90k {
		return a.Start90k < b.Start90k
	}
	if a.Track != b.Track {
		return a.Track < b.Track
	}
	if a.SampleIdx != b.SampleIdx {
		return a.SampleIdx < b.SampleIdx
	}
	return ida < idb
}

// Faults returns the accumulated job failures.
func (m *Merge) Faults() *fault.List {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := &fault.List{}
	for _, it := range m.error.Items {
		out.Add(it)
	}
	return out
}

// RunOptions configures a full pool run.
type RunOptions struct {
	// Items are the ordered units of work.
	Items []timeline.Entry
	// Work runs one entry; it must be pure with respect to shared state or
	// use its own locks.
	Work func(ctx context.Context, entry timeline.Entry) (any, error)
	// OnResult receives merged output; it is called from the calling
	// goroutine in canonical order, never concurrently.
	OnResult func(Merged) error
	// OnError receives per-item failures.
	OnError func(error)
}

// RunResult summarises a pool run.
type RunResult struct {
	Submitted int // jobs accepted by the queue
	Skipped   int // jobs never submitted because the run stopped
	Merged    int // jobs that produced a value before the run stopped
	Canceled  bool
	Faults    *fault.List
}

// Run feeds Items through a bounded pool and emits merged results in order.
// Cancellation is checked by the producer, by every worker through the shared
// context, and by the writer before each write. When the context is cancelled
// the queue is closed and no partially merged row is emitted.
func Run(ctx context.Context, cfg Config, opts RunOptions) (RunResult, error) {
	res := RunResult{Faults: &fault.List{}}
	pool, err := New(ctx, cfg)
	if err != nil {
		return res, err
	}
	merge := NewMerge()
	producerDone := make(chan struct{})

	// Sink and error callbacks run inside workers; the merge is internally
	// locked so this is safe.
	// The producer runs to completion (or cancellation) before Close, because
	// Close closes the job queue the producer sends on.
	go func() {
		defer close(producerDone)
		for id, item := range opts.Items {
			if pool.Canceled() {
				res.Skipped = len(opts.Items) - id
				pool.Cancel()
				return
			}
			// Capture the loop variables explicitly so every job sees its own
			// item under Go's per-iteration semantics and older toolchains.
			jobID, jobItem := id, item
			job := Job{
				ID: jobItem.SampleIdx,
				Run: func(ctx context.Context) (any, error) {
					if err := ctx.Err(); err != nil {
						return nil, fault.Wrap(-1, 0, fault.KindCancelled, true, err)
					}
					return opts.Work(ctx, jobItem)
				},
				Sink: func(v any) { merge.Add(jobID, jobItem, v) },
				Err: func(err error) {
					merge.AddError(err)
					if opts.OnError != nil {
						opts.OnError(err)
					}
				},
			}
			if !pool.Submit(job) {
				res.Skipped = len(opts.Items) - jobID
				pool.Cancel()
				return
			}
			res.Submitted++
		}
	}()

	// Close waits for workers, but Close must not be called before the
	// producer stops writing to jobs. Wait for the producer first, then close.
	<-producerDone
	pool.Close()

	// Cancellation is recorded explicitly by the producer and by the writer,
	// never inferred from the pool's own context cleanup.
	res.Canceled = res.Canceled || pool.wasCanceled()
	res.Merged = merge.Len()
	for _, it := range merge.Faults().Items {
		res.Faults.Add(it)
	}
	// Emission only happens for a non-cancelled run so a resumed run never
	// sees a half-written row.
	if res.Canceled {
		return res, fault.New(-1, 0, fault.KindCancelled, true,
			"run cancelled after %d of %d items", res.Submitted, len(opts.Items))
	}
	if opts.OnResult != nil {
		for _, m := range merge.Sorted() {
			if err := ctx.Err(); err != nil {
				res.Canceled = true
				return res, fault.Wrap(-1, 0, fault.KindCancelled, true, err)
			}
			if err := opts.OnResult(m); err != nil {
				return res, err
			}
		}
	}
	return res, nil
}
