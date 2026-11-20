// Package pipeline orchestrates the archive stages: streaming parse, segment
// windowing, timeline merge, deterministic render and durable sink.
//
// Every hand-off goes through a bounded channel, and a byte gate bounds the
// total in-flight memory: when a stage is slow, its full channel blocks the
// upstream sender instead of buffering without limit. Cancellation and sink
// failures unwind every goroutine, and a segment is reported only after its
// bytes have been written by the store, so acknowledged work is never lost.
package pipeline

import (
	"context"
	"sync"

	"archivetl/internal/clock"
	"archivetl/internal/diag"
	"archivetl/internal/mp4"
	"archivetl/internal/subtitle"
	"archivetl/internal/timeline"
)

// Default memory budget: queue capacity and per-record caps together keep
// the process under 64 MiB of in-flight data.
const (
	DefaultQueueCap     = 8
	DefaultMemoryBudget = 64 << 20 // 64 MiB
	recordOverheadBytes = 128
)

// SubtitleSource is one streaming input lane.
type SubtitleSource struct {
	Track    string
	Source   string
	Priority int
	Reader   interface {
		Read(p []byte) (int, error)
	}
}

// Config controls a run.
type Config struct {
	QueueCap      int
	MemoryBudget  int64
	SegmentTicks  clock.Tick // boundary between segment windows
	Horizon       clock.Tick // merge overflow horizon
	SlowConsumer  func()     // optional delay applied by the sink
	Output        SegmentStore
	Media         *mp4.Index // optional shared media index
	TrackPriority map[string]int
}

// SegmentStore is the durable sink abstraction implemented by the report
// package against the filesystem.
type SegmentStore interface {
	// HasMatchingOutput reports whether a previous output for id is still
	// byte-identical to the recorded input hash.
	HasMatchingOutput(id int, inputHash string) (bool, error)
	// WriteSegment persists payload and records its manifest entry.
	WriteSegment(seg *Segment, payload []byte) error
}

// Segment is one bounded time window with its merged lanes.
type Segment struct {
	ID            int
	Start         clock.Tick
	End           clock.Tick
	Records       []subtitle.Record
	Lanes         []timeline.Lane
	Summary       *timeline.Summary
	InputHash     string
	OutputHash    string
	KeyHits       int
	FailedOffsets []int64
	Skipped       bool
}

// Result is a segment the sink has durably committed (or confirmed unchanged).
type Result struct{ Segment *Segment }

// ErrorKind maps pipeline failures onto the CLI exit codes.
type ErrorKind int

const (
	KindConfig ErrorKind = iota + 1
	KindVerify
	KindIO
)

// RunError wraps a failure with its exit-code class.
type RunError struct {
	Kind  ErrorKind
	Inner error
}

func (e *RunError) Error() string { return e.Inner.Error() }
func (e *RunError) Unwrap() error { return e.Inner }

// memGate is a context-aware weighted semaphore built on sync.Cond.
type memGate struct {
	mu    sync.Mutex
	cond  *sync.Cond
	used  int64
	limit int64
}

func newMemGate(limit int64) *memGate {
	g := &memGate{limit: limit}
	g.cond = sync.NewCond(&g.mu)
	return g
}

func (g *memGate) acquire(ctx context.Context, n int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	for g.used+n > g.limit {
		stop := make(chan struct{})
		go func() {
			<-ctx.Done()
			g.cond.Broadcast()
		}()
		g.cond.Wait()
		close(stop)
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	g.used += n
	return nil
}

func (g *memGate) release(n int64) {
	g.mu.Lock()
	g.used -= n
	g.cond.Broadcast()
	g.mu.Unlock()
}

type recordEnvelope struct {
	rec  subtitle.Record
	size int64
}

// renderedEnvelope pairs a segment with its rendered payload on the way to
// the sink.
type renderedEnvelope struct {
	seg     *Segment
	payload []byte
	hash    string
}

// Run executes the full pipeline and returns committed segment results.
func Run(ctx context.Context, sources []SubtitleSource, cfg Config,
	renderer Renderer, coll *diag.Collector) ([]Result, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	gate := newMemGate(cfg.MemoryBudget)
	recordCh := make(chan recordEnvelope, cfg.QueueCap)
	segmentCh := make(chan *Segment, cfg.QueueCap)
	renderedCh := make(chan renderedEnvelope, cfg.QueueCap)
	resultCh := make(chan Result, cfg.QueueCap)
	errCh := make(chan error, 6)

	var wg sync.WaitGroup
	wg.Add(4)
	go produceRecords(runCtx, cancel, sources, recordCh, gate, coll, &wg, errCh)
	go windowSegments(runCtx, cancel, recordCh, segmentCh, gate, cfg, &wg, errCh)
	go renderStage(runCtx, cancel, segmentCh, renderedCh, cfg, renderer, &wg, errCh)
	go sinkStage(runCtx, cancel, renderedCh, resultCh, cfg, &wg, errCh)

	var committed []Result
	for r := range resultCh {
		committed = append(committed, r)
	}
	wg.Wait()
	close(errCh) // every producer has exited, so the channel is safe to close
	var first error
	for err := range errCh {
		if err != nil && first == nil {
			first = err
		}
	}
	if first != nil {
		return committed, classify(first)
	}
	if ctx.Err() != nil {
		return committed, &RunError{Kind: KindIO, Inner: ctx.Err()}
	}
	return committed, nil
}

func validateConfig(cfg Config) error {
	if cfg.QueueCap <= 0 || cfg.MemoryBudget <= 0 || cfg.SegmentTicks <= 0 {
		return &RunError{Kind: KindConfig, Inner: errInvalidConfig}
	}
	if cfg.Output == nil || cfg.SegmentTicks <= 0 {
		return &RunError{Kind: KindConfig, Inner: errInvalidConfig}
	}
	return nil
}

func classify(err error) error {
	if re, ok := err.(*RunError); ok {
		return re
	}
	return &RunError{Kind: KindIO, Inner: err}
}
