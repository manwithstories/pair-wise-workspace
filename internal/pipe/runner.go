package pipe

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"archivecheck/internal/index"
	"archivecheck/internal/parse"
	"archivecheck/internal/repair"
	"archivecheck/internal/verify"
)

// ErrClosed is returned when receiving from a closed queue.
var ErrClosed = errors.New("queue closed")

// CancelledError marks a run that stopped because its context was cancelled
// or because a deterministic interrupt point was reached.
type CancelledError struct {
	LastCommitted string
	Stage         string
	Err           error
}

func (e *CancelledError) Error() string {
	if e.LastCommitted == "" {
		return "processing cancelled before any fragment was committed (stage " + e.Stage + ")"
	}
	return "processing cancelled at stage " + e.Stage + " after committing " + e.LastCommitted
}

// Unwrap exposes the cancellation cause.
func (e *CancelledError) Unwrap() error { return e.Err }

// IsCancelled reports whether err represents an intentional cancellation.
func IsCancelled(err error) bool {
	var ce *CancelledError
	return errors.As(err, &ce)
}

// Options configures pipeline execution.
type Options struct {
	QueueCapacity int
	// SlowConsumer delays the transform stage before it consumes each parsed
	// fragment. Combined with the bounded queue it forces the producer to block.
	SlowConsumer time.Duration
	OnCommit     func(progress Progress) error
	// InterruptAfter stops after N committed fragments. Zero disables.
	InterruptAfter int
}

// Progress is emitted after each fragment is fully committed.
type Progress struct {
	Index        int
	Total        int
	Segment      string
	OutputBytes  int64
	QueueDepth   int
	PeakQueue    int64
	RepairedTfdt int
}

// FragmentEvent travels from the parse stage to the transform stage.
type FragmentEvent struct {
	Order    int
	Fragment *parse.Fragment
	File     *os.File
}

// TimelineEvent travels from the transform stage to the verify/write stage.
type TimelineEvent struct {
	Order    int
	Segment  string
	Entries  []repair.FragmentTimeline
	File     *os.File
	QueueLen int
	Peak     int64
}

// Start describes the committed state a run begins from.
type Start struct {
	InputDigest     string
	Resuming        bool
	Committed       int
	CommittedDigest string
	OutputBytes     int64
	NextDecodeTime  map[uint32]uint64
	Entries         []index.Entry
	RepairedTfdt    int
	MissingMfhd     int
}

// Result is the resumable state at the end of a run, successful or not.
type Result struct {
	InputDigest  string
	Checkpoint   *index.Checkpoint
	PeakQueue    int64
	CommittedAt  int
	OutputPath   string
	FullScan     *verify.Scan
	RepairedTfdt int
	MissingMfhd  int
}

// OutputPath returns the normalized output path for a work directory.
func OutputPath(workDir string) string { return workDir + string(os.PathSeparator) + "output.acx" }

// CheckpointPath returns the checkpoint path for a work directory.
func CheckpointPath(workDir string) string {
	return workDir + string(os.PathSeparator) + "checkpoint.json"
}

type stageFault struct {
	mu   sync.Mutex
	err  error
	stop context.CancelFunc
}

func (s *stageFault) record(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
	s.stop()
}

func (s *stageFault) get() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Run streams fragments through parse -> transform -> verify stages connected
// by bounded queues and writes a deterministic output plus checkpoint.
func Run(ctx context.Context, workDir string, init *parse.Init, fragments []*parse.Fragment, files map[string]*os.File, start Start, opts Options) (*Result, error) {
	if opts.QueueCapacity < 1 {
		return nil, fmt.Errorf("queue capacity must be at least 1")
	}
	if len(fragments) == 0 {
		return nil, fmt.Errorf("no fragments to process")
	}
	if start.Committed < 0 || start.Committed > len(fragments) {
		return nil, fmt.Errorf("resume boundary %d outside %d fragments", start.Committed, len(fragments))
	}
	res := &Result{OutputPath: OutputPath(workDir), CommittedAt: start.Committed,
		RepairedTfdt: start.RepairedTfdt, MissingMfhd: start.MissingMfhd,
		InputDigest: start.InputDigest}
	builder, err := repair.NewBuilder(init)
	if err != nil {
		return res, err
	}
	if start.Resuming {
		builder.Seed(start.NextDecodeTime)
	}
	var out *verify.Writer
	if start.Resuming {
		out, err = verify.ResumeWriter(res.OutputPath, start.CommittedDigest, start.OutputBytes)
	} else {
		out, err = verify.NewWriter(res.OutputPath)
	}
	if err != nil {
		return res, err
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	fault := &stageFault{stop: cancel}
	parseQ := NewQueue[FragmentEvent](opts.QueueCapacity)
	timelineQ := NewQueue[TimelineEvent](opts.QueueCapacity)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer parseQ.Close()
		produceStage(runCtx, parseQ, fragments, files, start.Committed, fault)
	}()
	go func() {
		defer wg.Done()
		defer timelineQ.Close()
		transformStage(runCtx, parseQ, timelineQ, builder, start.Committed, opts.SlowConsumer, fault)
	}()

	entries := append([]index.Entry(nil), start.Entries...)
	total := len(fragments)
	interrupted := false
	var commitErr error
	for pos := start.Committed; pos < total; pos++ {
		ev, rerr := timelineQ.Receive(runCtx)
		if rerr != nil {
			commitErr = rerr
			break
		}
		if ev.Order != pos {
			commitErr = fmt.Errorf("transform stage produced order %d, want %d", ev.Order, pos)
			fault.record(commitErr)
			break
		}
		var written []index.Entry
		for _, entry := range ev.Entries {
			es, werr := out.WriteFragment(entry, ev.File)
			if werr != nil {
				commitErr = werr
				fault.record(werr)
				break
			}
			written = append(written, es...)
		}
		if commitErr != nil {
			break
		}
		entries = append(entries, written...)
		res.CommittedAt = pos + 1
		if ev.Peak > res.PeakQueue {
			res.PeakQueue = ev.Peak
		}
		repaired, _ := builder.Counts()
		res.RepairedTfdt = start.RepairedTfdt + repaired
		if opts.OnCommit != nil {
			if cerr := opts.OnCommit(Progress{
				Index: res.CommittedAt, Total: total, Segment: ev.Segment,
				OutputBytes: out.Size(), QueueDepth: ev.QueueLen, PeakQueue: ev.Peak,
				RepairedTfdt: res.RepairedTfdt,
			}); cerr != nil {
				commitErr = cerr
				fault.record(cerr)
				break
			}
		}
		if opts.InterruptAfter > 0 && res.CommittedAt >= opts.InterruptAfter {
			interrupted = true
			break
		}
	}
	cancel()
	wg.Wait()

	stageErr := fault.get()
	if commitErr == nil && stageErr != nil && !errors.Is(stageErr, context.Canceled) && !errors.Is(stageErr, ErrClosed) {
		commitErr = stageErr
	}
	if commitErr != nil && !errors.Is(commitErr, ErrClosed) {
		// Persist every fully committed fragment before reporting the fault.
		if cp := res.seal(builder, out, entries, total); cp != nil {
			if err := index.Save(CheckpointPath(workDir), cp); err == nil {
				res.Checkpoint = cp
			}
		}
		out.Close()
		if errors.Is(commitErr, context.Canceled) || errors.Is(commitErr, context.DeadlineExceeded) || errors.Is(commitErr, ErrClosed) {
			return res, &CancelledError{LastCommitted: res.lastSegment(fragments), Stage: "transform", Err: commitErr}
		}
		return res, commitErr
	}
	if interrupted {
		cp := res.seal(builder, out, entries, total)
		if cp == nil {
			out.Close()
			return res, fmt.Errorf("cannot seal checkpoint")
		}
		if err := index.Save(CheckpointPath(workDir), cp); err != nil {
			out.Close()
			return res, err
		}
		res.Checkpoint = cp
		out.Close()
		return res, &CancelledError{LastCommitted: res.lastSegment(fragments), Stage: "interrupt", Err: context.Canceled}
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return res, err
	}
	cp := res.seal(builder, out, entries, total)
	if cp == nil {
		out.Close()
		return res, fmt.Errorf("cannot seal checkpoint")
	}
	digest, contentBytes, err := out.Finalize()
	if err != nil {
		return res, err
	}
	cp.CommittedDigest = digest
	cp.OutputBytes = contentBytes
	if err := index.Save(CheckpointPath(workDir), cp); err != nil {
		return res, err
	}
	res.Checkpoint = cp
	scan, err := verify.ScanFile(res.OutputPath, 0)
	if err != nil {
		return res, err
	}
	rep, err := verify.Check(res.OutputPath, cp)
	if err != nil {
		return res, err
	}
	if !rep.Verified {
		return res, fmt.Errorf("independent verification failed: %v", rep.Reasons)
	}
	res.FullScan = scan
	return res, nil
}

func produceStage(ctx context.Context, q *Queue[FragmentEvent], fragments []*parse.Fragment, files map[string]*os.File, skip int, fault *stageFault) {
	for i := skip; i < len(fragments); i++ {
		f := fragments[i]
		file := files[f.Segment]
		if file == nil {
			fault.record(fmt.Errorf("no open file for %s", f.Segment))
			return
		}
		if err := q.Send(ctx, FragmentEvent{Order: i, Fragment: f, File: file}); err != nil {
			if !errors.Is(err, context.Canceled) {
				fault.record(err)
			}
			return
		}
	}
}

func transformStage(ctx context.Context, in *Queue[FragmentEvent], out *Queue[TimelineEvent], builder *repair.Builder, skip int, slow time.Duration, fault *stageFault) {
	for {
		if slow > 0 {
			select {
			case <-time.After(slow):
			case <-ctx.Done():
				return
			}
		}
		ev, err := in.Receive(ctx)
		if err != nil {
			return
		}
		entries, err := builder.Add(ev.Fragment)
		if err != nil {
			fault.record(err)
			return
		}
		te := TimelineEvent{
			Order: ev.Order, Segment: ev.Fragment.Segment, Entries: entries, File: ev.File,
			QueueLen: in.Len(), Peak: in.Peak(),
		}
		if err := out.Send(ctx, te); err != nil {
			if !errors.Is(err, context.Canceled) {
				fault.record(err)
			}
			return
		}
	}
}

func (r *Result) lastSegment(fragments []*parse.Fragment) string {
	if r.CommittedAt <= 0 || r.CommittedAt > len(fragments) {
		return ""
	}
	return fragments[r.CommittedAt-1].Segment
}

func (r *Result) seal(builder *repair.Builder, out *verify.Writer, entries []index.Entry, total int) *index.Checkpoint {
	idx, err := index.Normalize(entries)
	if err != nil {
		return nil
	}
	names := make([]string, 0, 16)
	seen := map[string]bool{}
	for _, e := range entries {
		if !seen[e.Segment] {
			seen[e.Segment] = true
			names = append(names, e.Segment)
		}
	}
	cp := &index.Checkpoint{
		Version:            index.CheckpointVersion,
		InputDigest:        r.InputDigest,
		Committed:          r.CommittedAt,
		CommittedDigest:    out.Digest(),
		CommittedFragments: names,
		FragmentsTotal:     total,
		RepairedTfdt:       r.RepairedTfdt,
		MissingMfhd:        r.MissingMfhd,
		OutputBytes:        out.Size(),
		NextDecodeTime:     builder.Next(),
		Index:              idx,
	}
	if len(names) > 0 {
		cp.LastCommitted = names[len(names)-1]
	}
	cp.RepairedTfdt = r.RepairedTfdt
	return cp
}
