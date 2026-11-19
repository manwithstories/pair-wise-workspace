// Package pipeline orchestrates the mediq data flow:
//
//	reader goroutine -> bounded queue -> validate -> timeline repair -> index
//
// The reader goroutine only parses records; validation, repair, index writing
// and checkpointing all happen on the consumer goroutine, so no shared mutable
// state needs locking. The bounded queue applies backpressure: a slow consumer
// stops the reader instead of letting memory grow. Cancellation closes one
// context and both goroutines exit.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"mediq/internal/checkpoint"
	"mediq/internal/model"
	"mediq/internal/source"
	"mediq/internal/timeline"
)

// Options configures a run.
type Options struct {
	Input          string
	Output         string
	CheckpointPath string
	ChunkFrames    int
	QueueDepth     int
	TimelineOpts   timeline.Options
	// Restart discards any existing checkpoint and starts from byte zero.
	Restart bool
	// SlowConsumer adds an artificial delay per sample, used by the fixture
	// demonstration to prove that backpressure bounds memory.
	SlowConsumer time.Duration
	// Log receives human-readable progress lines. Nil discards them.
	Log io.Writer
}

func (o Options) withDefaults() Options {
	if o.ChunkFrames <= 0 {
		o.ChunkFrames = 256
	}
	if o.QueueDepth <= 0 {
		o.QueueDepth = 8
	}
	if o.CheckpointPath == "" {
		o.CheckpointPath = o.Output + ".ckpt"
	}
	if o.Log == nil {
		o.Log = io.Discard
	}
	return o
}

// Stats is the terminal report for a run.
type Stats struct {
	FramesRead         uint64
	FramesIndexed      uint64
	FramesBad          uint64
	FramesQuarantined  uint64
	FirstCorruptFrame  uint64
	FirstCorruptOffset int64
	HasCorrupt         bool
	RecoveredChunks    uint64
	ResumeMs           int64
	PeakQueueDepth     int
	ResumeUsable       bool
	OutputSHA256       string
	IndexValid         bool
	Resumed            bool
	ResumeOffset       int64
	InputSize          int64
	QuarantineReasons  map[string]int
}

// Runner executes one job.
type Runner struct {
	opts Options
	cks  *checkpoint.Store
	log  func(format string, args ...any)
}

func New(opts Options) *Runner {
	o := opts.withDefaults()
	return &Runner{
		opts: o,
		cks:  checkpoint.New(o.CheckpointPath),
		log:  func(f string, a ...any) { fmt.Fprintf(o.Log, f+"\n", a...) },
	}
}

// Run executes the pipeline.
func (r *Runner) Run(ctx context.Context) (*Stats, error) {
	started := time.Now()
	stats := &Stats{QuarantineReasons: map[string]int{}}
	if err := r.validatePaths(); err != nil {
		return nil, err
	}
	fi, err := os.Stat(r.opts.Input)
	if err != nil {
		return nil, fmt.Errorf("input: %w", err)
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("input %s is a directory", r.opts.Input)
	}
	stats.InputSize = fi.Size()

	st, err := r.prepareCheckpoint(fi.Size())
	if err != nil {
		return nil, err
	}
	idx, err := r.openIndex(st)
	if err != nil {
		return nil, err
	}
	// The teardown action depends on why Run returns: a failure discards the
	// part file, while cancellation keeps it so the checkpoint can resume.
	var teardown func()
	defer func() {
		if teardown != nil {
			teardown()
		}
	}()

	job := &job{
		runner:    r,
		stats:     stats,
		idx:       idx,
		repairer:  timeline.New(r.opts.TimelineOpts),
		started:   started,
		committed: 0,
	}
	resumeOffset := int64(0)
	if st != nil {
		job.repairer.Restore(st.Timeline)
		job.chunks = append([]checkpoint.ChunkRecord(nil), st.Chunks...)
		job.indexed = st.Index.Samples
		job.readOffset = st.CommittedOffset
		job.committed = st.CommittedOffset
		job.replayLimit = st.CommittedOffset
		stats.FramesRead = st.FramesRead
		stats.FramesBad = st.FramesBad
		stats.FramesQuarantined = st.FramesQuarantined
		stats.ResumeOffset = st.CommittedOffset
		stats.Resumed = true
		stats.RecoveredChunks = uint64(len(st.Chunks))
		resumeOffset = st.CommittedOffset
	}

	src, err := source.Open(r.opts.Input, source.Options{})
	if err != nil {
		return nil, err
	}
	defer src.Close()
	if resumeOffset > 0 {
		if err := src.SeekStart(resumeOffset); err != nil {
			return nil, err
		}
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	queue := make(chan source.Record, r.opts.QueueDepth)
	var wg sync.WaitGroup
	var peak atomic.Int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(queue)
		r.pump(runCtx, src, queue, &peak)
	}()

	runErr := job.consume(runCtx, queue)
	cancel()
	wg.Wait()
	stats.PeakQueueDepth = int(peak.Load())

	if runErr != nil {
		idx.CloseWithoutPublishing()
		idx = nil
		if errors.Is(runErr, context.Canceled) {
			// The last committed chunk is on disk and the checkpoint names it.
			stats.ResumeUsable = true
		}
		return stats, runErr
	}
	if !job.sourceDone {
		idx.Abort()
		idx = nil
		return stats, errors.New("index incomplete: source was not fully consumed")
	}
	teardown = idx.Abort // used only if finish fails
	if err := job.finishOnDisk(idx); err != nil {
		return stats, err
	}
	teardown = nil // Finish/Publish released the part file and tables
	idx = nil
	stats.ResumeMs = time.Since(started).Milliseconds()
	return stats, nil
}

// pump reads records into the bounded queue. It is the only producer
// goroutine; it exits on cancellation, a read error or end of file.
func (r *Runner) pump(ctx context.Context, src *source.Reader, queue chan<- source.Record, peak *atomic.Int64) {
	for {
		rec, err := src.Read()
		if err != nil {
			return
		}
		if d := int64(len(queue)); d > peak.Load() {
			peak.Store(d)
		}
		select {
		case queue <- rec:
		case <-ctx.Done():
			return
		}
		if rec.Fault != nil && !rec.Fault.Resync() {
			return
		}
	}
}

func (r *Runner) validatePaths() error {
	abs := func(p string) string {
		if p == "" {
			return p
		}
		a, err := filepath.Abs(p)
		if err != nil {
			return p
		}
		return filepath.Clean(a)
	}
	input := abs(r.opts.Input)
	for name, p := range map[string]string{
		"output":          r.opts.Output,
		"checkpoint":      r.opts.CheckpointPath,
		"output part":     r.opts.Output + ".part",
		"checkpoint part": r.opts.CheckpointPath + ".part",
	} {
		if p == "" {
			continue
		}
		if abs(p) == input {
			return fmt.Errorf("%s path %s collides with the input; refusing to overwrite it", name, p)
		}
	}
	if abs(r.opts.Output) == abs(r.opts.CheckpointPath) {
		return fmt.Errorf("output and checkpoint paths must differ")
	}
	return nil
}

var _ = model.ErrCanceled
