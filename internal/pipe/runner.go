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
	NextDecodeTime  map[string]uint64
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
func Run(ctx context.Context, workDir string, tasks []Task, l *LoadedInput, start Start, opts Options) (*Result, error) {
	if opts.QueueCapacity < 1 {
		return nil, fmt.Errorf("queue capacity must be at least 1")
	}
	if len(tasks) == 0 {
		return nil, fmt.Errorf("no fragments to process")
	}
	if start.Committed < 0 || start.Committed > len(tasks) {
		return nil, fmt.Errorf("resume boundary %d outside %d fragments", start.Committed, len(tasks))
	}
	if start.Committed == len(tasks) {
		// Nothing left to do. Leave the completed output untouched: appending
		// another trailer would corrupt it, and re-sealing would rotate its
		// checksum's meaning.
		res := &Result{OutputPath: OutputPath(workDir), CommittedAt: start.Committed,
			RepairedTfdt: start.RepairedTfdt, MissingMfhd: start.MissingMfhd,
			InputDigest: start.InputDigest}
		if start.Resuming {
			if cp, err := index.Load(CheckpointPath(workDir)); err == nil {
				res.Checkpoint = cp
			}
		}
		return res, nil
	}
	res := &Result{OutputPath: OutputPath(workDir), CommittedAt: start.Committed,
		RepairedTfdt: start.RepairedTfdt, MissingMfhd: start.MissingMfhd,
		InputDigest: start.InputDigest}
	builders := make(map[int]*repair.Builder, len(l.Inits))
	for gi := range l.Inits {
		track := l.TrackFor(gi)
		b, err := repair.NewBuilder(gi, track)
		if err != nil {
			return res, err
		}
		if start.Resuming {
			if next, ok := start.NextDecodeTime[CursorKey(gi, track.ID)]; ok {
				b.Seed(next, true)
			}
		}
		builders[gi] = b
	}
	var out *verify.Writer
	var err error
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
		produceStage(runCtx, parseQ, tasks, l, start.Committed, fault)
	}()
	go func() {
		defer wg.Done()
		defer timelineQ.Close()
		transformStage(runCtx, parseQ, timelineQ, builders, opts.SlowConsumer, fault)
	}()

	entries := append([]index.Entry(nil), start.Entries...)
	nextBase := make(map[string]uint64, len(start.NextDecodeTime))
	for k, v := range start.NextDecodeTime {
		nextBase[k] = v
	}
	repairedCommitted := start.RepairedTfdt
	total := len(tasks)
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
		spanStart := out.Size()
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
		// Verify stage: independently re-read the just-written span before the
		// committed boundary advances. A mismatch fails the run without
		// publishing a checkpoint that claims the data is good.
		reRead, verr := verify.ScanSpan(out.ReaderAt(), spanStart, out.Size(), res.OutputPath)
		if verr == nil {
			verr = verify.Verify(written, reRead)
		}
		if verr != nil {
			commitErr = verr
			fault.record(verr)
			break
		}
		entries = append(entries, written...)
		res.CommittedAt = pos + 1
		if ev.Peak > res.PeakQueue {
			res.PeakQueue = ev.Peak
		}
		// Counters and the resume cursor advance only for committed data, so
		// fragments still in flight when the run is cut short never leak into
		// the checkpoint.
		for _, ft := range ev.Entries {
			if ft.Reason == repair.ReasonBackward {
				repairedCommitted++
			}
			last := nextBase[CursorKey(ft.Group, ft.TrackID)]
			if len(ft.Samples) > 0 {
				last = ft.Samples[len(ft.Samples)-1].EndDTS
			} else {
				last = ft.RepairedBase
			}
			nextBase[CursorKey(ft.Group, ft.TrackID)] = last
		}
		res.RepairedTfdt = repairedCommitted
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
	// A real stage fault always wins over the cancellation it triggered, so a
	// truncated or malformed input is reported as input failure, not exit 4.
	if stageErr != nil && !errors.Is(stageErr, context.Canceled) && !errors.Is(stageErr, ErrClosed) {
		commitErr = stageErr
	}
	if commitErr != nil && !errors.Is(commitErr, ErrClosed) {
		// Preserve the best committed checkpoint. A fault never regresses an
		// existing checkpoint: if this run committed less than what is already
		// on disk, the older (larger) boundary stays authoritative.
		if err := out.Sync(); err == nil && res.CommittedAt > 0 {
			if cp := res.seal(out, entries, nextBase, total); cp != nil {
				if prev, perr := index.Load(CheckpointPath(workDir)); perr == nil && prev.Committed >= cp.Committed {
					res.Checkpoint = prev
				} else if err := index.Save(CheckpointPath(workDir), cp); err == nil {
					res.Checkpoint = cp
				}
			}
		}
		out.Close()
		if errors.Is(commitErr, context.Canceled) || errors.Is(commitErr, context.DeadlineExceeded) || errors.Is(commitErr, ErrClosed) {
			return res, &CancelledError{LastCommitted: res.lastSegment(tasks), Stage: "transform", Err: commitErr}
		}
		return res, commitErr
	}
	if interrupted {
		if err := out.Sync(); err != nil {
			out.Close()
			return res, err
		}
		cp := res.seal(out, entries, nextBase, total)
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
		return res, &CancelledError{LastCommitted: res.lastSegment(tasks), Stage: "interrupt", Err: context.Canceled}
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return res, err
	}
	cp := res.seal(out, entries, nextBase, total)
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

func produceStage(ctx context.Context, q *Queue[FragmentEvent], tasks []Task, l *LoadedInput, skip int, fault *stageFault) {
	for i := skip; i < len(tasks); i++ {
		task := tasks[i]
		file := l.Files[task.Segment]
		if file == nil {
			fault.record(fmt.Errorf("no open file for %s", task.Segment))
			return
		}
		owner := l.InitFor(task.Segment)
		if owner == nil {
			fault.record(fmt.Errorf("no init segment owns %s", task.Segment))
			return
		}
		info, err := file.Stat()
		if err != nil {
			fault.record(err)
			return
		}
		frag, err := parse.ParseFragmentReaderInGroup(task.Group, file, info.Size(), task.Segment, owner)
		if err != nil {
			fault.record(err)
			return
		}
		if err := q.Send(ctx, FragmentEvent{Order: i, Fragment: frag, File: file}); err != nil {
			if !errors.Is(err, context.Canceled) {
				fault.record(err)
			}
			return
		}
	}
}

func transformStage(ctx context.Context, in *Queue[FragmentEvent], out *Queue[TimelineEvent], builders map[int]*repair.Builder, slow time.Duration, fault *stageFault) {
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
		builder := builders[ev.Fragment.Group]
		if builder == nil {
			fault.record(fmt.Errorf("no timeline builder for group %d", ev.Fragment.Group))
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

func (r *Result) lastSegment(tasks []Task) string {
	if r.CommittedAt <= 0 || r.CommittedAt > len(tasks) {
		return ""
	}
	return tasks[r.CommittedAt-1].Segment
}

func (r *Result) seal(out *verify.Writer, entries []index.Entry, nextBase map[string]uint64, total int) *index.Checkpoint {
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
		NextDecodeTime:     nextBase,
		Index:              idx,
	}
	if len(names) > 0 {
		cp.LastCommitted = names[len(names)-1]
	}
	cp.RepairedTfdt = r.RepairedTfdt
	return cp
}
