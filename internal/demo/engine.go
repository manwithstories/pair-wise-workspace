package demo

import (
	"context"
	"fmt"
	"sync"

	"mediascan/internal/checkpoint"
	"mediascan/internal/fault"
	"mediascan/internal/mp4"
	"mediascan/internal/pipeline"
	"mediascan/internal/report"
)

// Exit codes mirror the acceptance contract.
const (
	ExitOK            = 0
	ExitCorrupt       = 2
	ExitInvalidParams = 3
	ExitCanceled      = 4
	ExitCheckpoint    = 5
)

// Result is the outcome of one engine run.
type Result struct {
	Summary    report.Summary
	Faults     []fault.Fault
	Canceled   bool
	ResumeUsed bool
	// ResumeMs measures wall time spent recomputing segments on a resume.
	ResumeMs int64
}

// RunConfig controls a single scan invocation.
type RunConfig struct {
	Media        *Media
	Checkpoint   string
	Resume       bool
	CancelAfter  int64 // cancel once this many frames are processed; <=0 = off
	Pool         pipeline.Limits
}

// Engine runs the deterministic scan with checkpoint/resume support.
type Engine struct {
	media *Media
	spec  Spec
	lim   mp4.Limits
}

// NewEngine validates the media and prepares an engine.
func NewEngine(m *Media) (*Engine, error) {
	if m == nil {
		return nil, fmt.Errorf("demo: nil media")
	}
	if err := m.Spec.Validate(); err != nil {
		return nil, err
	}
	return &Engine{media: m, spec: m.Spec, lim: mp4.DefaultLimits()}, nil
}

// segmentCount is the number of fixed-size work segments.
func (e *Engine) segmentCount() int64 {
	n := int64(e.spec.Frames) / e.spec.SegmentFrames
	if int64(e.spec.Frames)%e.spec.SegmentFrames != 0 {
		n++
	}
	return n
}

// Run executes the scan. It first loads (or creates) a checkpoint, then
// processes only the segments not already completed.
func (e *Engine) Run(ctx context.Context, cfg RunConfig) (*Result, error) {
	paramsDigest := checkpoint.DigestParams(e.spec.CanonicalParams())
	inputDigest := e.media.Digest

	var store *checkpoint.Store
	if cfg.Resume {
		loaded, err := checkpoint.Load(cfg.Checkpoint, inputDigest, paramsDigest)
		if err != nil {
			return nil, fmt.Errorf("exit %d: %w", ExitCheckpoint, err)
		}
		store = loaded
	} else {
		store = checkpoint.New(inputDigest, paramsDigest)
	}

	// Pre-scan the intact container once: validates boxes and resolves all
	// per-sample timing on the unified grid.
	roots, err := mp4.ScanBoxes(e.media.Bytes, e.lim)
	if err != nil {
		return nil, fmt.Errorf("exit %d: container scan: %w", ExitCorrupt, err)
	}
	tracks, _, err := mp4.BuildTracks(e.media.Bytes, roots, e.lim)
	if err != nil {
		return nil, fmt.Errorf("exit %d: track build: %w", ExitCorrupt, err)
	}
	_ = tracks

	totalSeg := e.segmentCount()
	pool, err := pipeline.NewPool(cfg.Pool)
	if err != nil {
		return nil, fmt.Errorf("exit %d: %w", ExitInvalidParams, err)
	}

	// Internal cancel fires when CancelAfter frames are done; it is derived
	// from the caller context so an external cancel still propagates.
	runCtx, internalCancel := context.WithCancel(ctx)
	defer internalCancel()

	// segResult carries one segment's computed findings back from a worker.
	type segResult struct {
		id      int64
		start   int64
		end     int64
		faults  []fault.Fault
		partial string
	}

	// Build the list of segments that still need work, in ascending order.
	type segRange struct{ id, start, end int64 }
	var pending []segRange
	for seg := int64(0); seg < totalSeg; seg++ {
		start := seg * e.spec.SegmentFrames
		end := start + e.spec.SegmentFrames
		if end > int64(e.spec.Frames) {
			end = int64(e.spec.Frames)
		}
		if !store.IsSegmentDone(seg) {
			pending = append(pending, segRange{seg, start, end})
		}
	}

	resultsCh := make(chan segResult, cfg.Pool.MaxQueueLen)
	jobs := make([]pipeline.Job, 0, len(pending))
	for _, pr := range pending {
		pr := pr
		jobs = append(jobs, pipeline.Job{
			Seq: pr.id, SegmentID: pr.id,
			Work: func(runCtx context.Context) error {
				if err := runCtx.Err(); err != nil {
					return err
				}
				fs := e.scanSegment(pr.start, pr.end)
				partial := checkpoint.DigestParams(report.Canonical(
					e.reportParams(), pipeline.MergeFaults(fs)))
				select {
				case resultsCh <- segResult{pr.id, pr.start, pr.end, fs, partial}:
					return nil
				case <-runCtx.Done():
					return runCtx.Err()
				}
			},
		})
	}

	// Single committer: reorders worker results by segment ID and appends to
	// the checkpoint strictly in order, so persistence stays deterministic.
	var (
		wg        sync.WaitGroup
		canceled  bool
		processed int64
		commitErr error
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		next := int64(0)
		if len(store.Segments) > 0 {
			next = store.Segments[len(store.Segments)-1].ID + 1
		}
		buf := make(map[int64]segResult)
		for {
			select {
			case <-runCtx.Done():
				return
			case r, ok := <-resultsCh:
				if !ok {
					return
				}
				buf[r.id] = r
				for {
					ready, found := buf[next]
					if !found {
						break
					}
					delete(buf, next)
					if err := store.Append(checkpoint.Segment{
						ID: ready.id, Start: ready.start, End: ready.end,
						PartialDigest: ready.partial, Faults: ready.faults,
					}, cfg.Checkpoint); err != nil {
						commitErr = err
						internalCancel()
						return
					}
					processed += ready.end - ready.start
					if cfg.CancelAfter > 0 && processed >= cfg.CancelAfter {
						canceled = true
						internalCancel()
						return
					}
					next++
				}
			}
		}
	}()

	runErr := pool.Run(runCtx, jobs)
	// The pool finishing means no more results will be sent.
	close(resultsCh)
	wg.Wait()
	if commitErr != nil {
		return nil, commitErr
	}
	if canceled {
		runErr = context.Canceled
	}

	// Rebuild the complete fault list from all persisted segments so a
	// resumed run reconstructs exactly the full-run ordering.
	var allFaults [][]fault.Fault
	for _, seg := range store.Segments {
		allFaults = append(allFaults, seg.Faults)
	}
	merged := pipeline.MergeFaults(allFaults...)

	if canceled || isCancel(runErr) {
		return &Result{
			Faults:   merged,
			Canceled: true,
			Summary: report.Build(e.reportParams(), e.spec.Tracks, e.spec.Frames,
				merged, 0, processed),
		}, nil
	}
	if runErr != nil {
		return nil, runErr
	}

	summary := report.Build(e.reportParams(), e.spec.Tracks, e.spec.Frames,
		merged, 0, processed)
	return &Result{Summary: summary, Faults: merged, ResumeUsed: cfg.Resume}, nil
}

// reportParams adapts the demo spec to the report parameter set.
func (e *Engine) reportParams() report.Params {
	return report.Params{
		Seed: e.spec.Seed, Tracks: e.spec.Tracks, Frames: e.spec.Frames,
		CorruptAt: e.spec.CorruptAt, Timescale: e.spec.Timescale,
		MaxKeyGap: e.spec.KeyGapLimit, DriftLimit: e.spec.DriftLimit,
	}
}

func isCancel(err error) bool {
	return err == context.Canceled || err == context.DeadlineExceeded
}
