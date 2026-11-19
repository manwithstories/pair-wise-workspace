package pipeline

import (
	"context"
	"sync"
	"time"

	"mediapipeline/internal/clock"
	"mediapipeline/internal/mediaerr"
	"mediapipeline/internal/reorder"
	"mediapipeline/internal/stream"
	"mediapipeline/internal/testdata"
)

// FatalFunc reports an unrecoverable stage error; the first call cancels the run.
type FatalFunc func(*mediaerr.Error)

// Counters aggregates the quantified counters emitted across all stages.
type Counters struct {
	Datagrams       int64
	RTCPValid       int64
	RTCPInvalid     int64
	ParseErrors     int64
	Parsed          int64
	Dropped         int64 // sequence numbers declared lost
	Late            int64
	Duplicates      int64
	Conflicts       int64
	Gaps            int64
	Isolated        int64
	PartialFailures int64
}

// SegmentResult is the quantified outcome for one output segment.
type SegmentResult struct {
	Index           int
	StartNS         int64
	EndNS           int64
	SHA256          string
	RoundTrip       float64
	KeyframeHitRate float64
	AudioSamples    int
	VideoSamples    int
	PartialFailure  bool
	FailureReasons  []string
}

// Result is the full pipeline run outcome.
type Result struct {
	// counterMu guards Counters, which every stage goroutine increments.
	counterMu       sync.Mutex
	Counters        Counters
	PeakBufferBytes int64
	// Failures holds structured errors for isolated (non-fatal) faults.
	Failures []*mediaerr.Error
	// Segments is appended only by the consumer stage, so it needs no lock.
	Segments []SegmentResult
}

// add increments one counter field under the result lock.
func (res *Result) add(sel func(*Counters)) {
	res.counterMu.Lock()
	sel(&res.Counters)
	res.counterMu.Unlock()
}

// SnapshotCounters returns a copy safe to read after the run.
func (res *Result) SnapshotCounters() Counters {
	res.counterMu.Lock()
	defer res.counterMu.Unlock()
	return res.Counters
}

// AudioSpec fixes the negotiated audio output specification.
type AudioSpec struct {
	ClockRate     uint32
	TargetRate    int
	TargetLayout  string
	PeakCeiling   float64
	WindowSamples int
}

// DefaultAudioSpec returns the negotiation used by demo and verify.
func DefaultAudioSpec() AudioSpec {
	return AudioSpec{
		ClockRate: 48000, TargetRate: 48000, TargetLayout: "stereo",
		PeakCeiling: 0.9, WindowSamples: 480,
	}
}

// RunOptions controls a pipeline run beyond the queue bounds.
type RunOptions struct {
	SegmentNS       int64
	ConsumerDelayNS int64
	Audio           AudioSpec
	KeyframeEvery   int
}

// Runner executes the stage graph for one fixture.
type Runner struct {
	cfg  Config
	opts RunOptions
	// stateMu guards the per-track maps, which are lazily populated by the
	// producer and reorder stages running on separate goroutines.
	stateMu  sync.RWMutex
	trackers map[uint32]*stream.Tracker
	reorders map[uint32]*reorder.Buffer
	tlines   map[uint32]*clock.Timeline
}

// NewRunner validates configuration and builds per-track state.
func NewRunner(cfg Config, opts RunOptions) (*Runner, *mediaerr.Error) {
	if merr := cfg.Validate(); merr != nil {
		return nil, merr
	}
	if opts.SegmentNS <= 0 {
		return nil, mediaerr.New(mediaerr.StagePipeline, mediaerr.KindInvalidInput,
			"SegmentNS must be positive")
	}
	return &Runner{
		cfg:      cfg,
		opts:     opts,
		trackers: make(map[uint32]*stream.Tracker),
		reorders: make(map[uint32]*reorder.Buffer),
		tlines:   make(map[uint32]*clock.Timeline),
	}, nil
}

// Run drives the fixture through every stage. Isolated faults are collected in
// Result.Failures and counted, never aborting the run; only cancellation or an
// unrecoverable stage error returns a non-nil error.
func (r *Runner) Run(ctx context.Context, fx *testdata.Fixture) (*Result, *mediaerr.Error) {
	// Two queues share the memory budget so the summed high-water mark can
	// never exceed the configured byte cap.
	qCap := r.cfg.MaxBufferBytes / 2
	pCap := r.cfg.MaxBufferPackets / 2
	if pCap < 1 {
		pCap = 1
	}
	parseQ := newBoundedQueue(qCap, pCap)
	processQ := newBoundedQueue(qCap, pCap)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	res := &Result{}
	var failMu sync.Mutex
	addFailure := func(e *mediaerr.Error) {
		failMu.Lock()
		res.Failures = append(res.Failures, e)
		failMu.Unlock()
	}

	var wg sync.WaitGroup
	// Buffered for the three stages: a fatal error cancels runCtx, which
	// releases every blocked stage so they can all drain without deadlocking.
	var fatalCh = make(chan *mediaerr.Error, 3)
	var fatalOnce sync.Once
	// First fatal error cancels the whole run so downstream stages stop and
	// no stage keeps sending into a queue another stage is about to close.
	reportFatal := func(e *mediaerr.Error) {
		if e == nil {
			return
		}
		fatalOnce.Do(func() {
			fatalCh <- e
			cancel()
		})
	}

	// Stage 1: demux + parse + sequence tracking.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer parseQ.Close()
		r.stageProduce(runCtx, fx, parseQ, res, addFailure, reportFatal)
	}()

	// Stage 2: bounded reorder with per-track windows.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer processQ.Close()
		r.stageReorder(runCtx, parseQ, processQ, res, addFailure, reportFatal)
	}()

	// Stage 3: clock, audio processing, segmentation and MP4 output.
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.stageConsume(runCtx, processQ, fx, res, addFailure, reportFatal)
	}()

	wg.Wait()
	close(fatalCh)
	for e := range fatalCh {
		if e != nil {
			r.finalizeCounters(res)
			res.PeakBufferBytes = parseQ.PeakBytes() + processQ.PeakBytes()
			return res, e
		}
	}
	r.finalizeCounters(res)
	res.PeakBufferBytes = parseQ.PeakBytes() + processQ.PeakBytes()
	res.Counters.PartialFailures = int64(len(res.Failures))
	return res, nil
}

// send applies the per-send slow-consumer timeout by deriving a child
// context; the bounded queue already honors context cancellation, so no extra
// goroutine is spawned and the single-sender invariant is preserved.
func (r *Runner) send(q *boundedQueue, ctx context.Context, p *stream.Packet) *mediaerr.Error {
	if r.cfg.ConsumerTimeoutNS <= 0 {
		return q.Send(ctx, p)
	}
	sendCtx, sendCancel := context.WithTimeout(ctx, time.Duration(r.cfg.ConsumerTimeoutNS))
	defer sendCancel()
	if merr := q.Send(sendCtx, p); merr != nil {
		// Distinguish the local timeout from outer cancellation.
		if ctx.Err() == nil && sendCtx.Err() == context.DeadlineExceeded {
			return mediaerr.ErrTimeout
		}
		return merr
	}
	return nil
}

// finalizeCounters folds tracker loss/duplicate stats into the result. The
// reorder-stage counters (late/gaps/isolated) are already recorded inline, so
// they are not accumulated again here.
func (r *Runner) finalizeCounters(res *Result) {
	r.stateMu.RLock()
	defer r.stateMu.RUnlock()
	var dropped, dup, conflict int64
	for _, tr := range r.trackers {
		s := tr.Flush()
		dropped += s.Lost
		dup += s.Duplicates
		conflict += s.Conflicts
	}
	res.add(func(c *Counters) {
		c.Dropped += dropped
		c.Duplicates += dup
		c.Conflicts += conflict
	})
}
