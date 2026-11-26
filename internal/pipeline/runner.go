package pipeline

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"aircheck/internal/media"
)

// Source yields the segments to process. Implementations read from the local
// fixture store only.
type Source interface {
	// Len reports the segment count.
	Len() int
	// Segment reads one segment's bytes and identity.
	Segment(i int) (SegmentData, error)
}

// Runner drives blocks through the bounded queue and feeds whole segments to
// the processor. It guarantees:
//
//   - memory held by the queue never exceeds MaxBytes;
//   - a slow consumer blocks the producer instead of dropping data;
//   - after cancellation no partial segment is committed: the runner finishes
//     the segment boundary it is on, then stops and reports where it stopped.
type Runner struct {
	opt   RunnerOptions
	store *Store
	queue *Queue
	proc  Processor
	src   Source
}

// NewRunner validates options and prepares the store.
func NewRunner(opt RunnerOptions, src Source, proc Processor) (*Runner, error) {
	if opt.BlockBytes == 0 {
		opt.BlockBytes = DefaultBlockBytes
	}
	if opt.MaxBytes == 0 {
		opt.MaxBytes = DefaultMaxBufferBytes
	}
	q, err := NewQueue(opt.BlockBytes, opt.MaxBytes)
	if err != nil {
		return nil, err
	}
	if opt.OutDir == "" {
		return nil, media.NewFault(media.FaultStateInvariant, media.SourcePos{}, "runner: output directory required")
	}
	store, err := OpenStore(opt.OutDir)
	if err != nil {
		return nil, err
	}
	if src == nil || proc == nil {
		return nil, media.NewFault(media.FaultStateInvariant, media.SourcePos{}, "runner: source and processor required")
	}
	return &Runner{opt: opt, store: store, queue: q, proc: proc, src: src}, nil
}

// Store exposes the commit area (used by the manifest writer).
func (r *Runner) Store() *Store { return r.store }

// Run processes all segments. The producer reads and splits one segment at a
// time, blocking on the queue; the consumer reassembles blocks and processes
// whole segments. Cancellation stops the producer at the next segment start,
// so the queue always drains in whole segments.
func (r *Runner) Run(ctx context.Context) (*Report, error) {
	start := time.Now()
	rep := &Report{}
	var wg sync.WaitGroup
	var firstErr atomic.Value
	var stopProducer atomic.Bool

	// Producer: streams blocks into the queue.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer r.queue.Close()
		for i := 0; i < r.src.Len(); i++ {
			if ctx.Err() != nil {
				stopProducer.Store(true)
				return
			}
			seg, err := r.src.Segment(i)
			if err != nil {
				firstErr.Store(err)
				stopProducer.Store(true)
				return
			}
			blocks := Split(seg.Index, seg.Seq, seg.URI, seg.Duration, seg.Data, r.opt.BlockBytes)
			for _, b := range blocks {
				if err := r.queue.Put(ctx, b); err != nil {
					if err == ErrCancelled {
						stopProducer.Store(true)
						return
					}
					firstErr.Store(media.WrapFault(media.FaultStateInvariant,
						media.SourcePos{Artifact: seg.URI, Segment: seg.Index}, err))
					stopProducer.Store(true)
					return
				}
			}
		}
	}()

	// Consumer: reassembles blocks into segments and processes them.
	wg.Add(1)
	go func() {
		defer wg.Done()
		var parts [][]byte
		var curBlock *Block
		for {
			b, err := r.queue.Get(ctx)
			if err == ErrClosed {
				break
			}
			if err == ErrCancelled {
				// Cancellation is only observed at a block boundary; the
				// current segment is abandoned whole, never half-written.
				break
			}
			if curBlock == nil {
				curBlock = b
			}
			parts = append(parts, b.Data)
			if !b.Last {
				continue
			}
			seg := SegmentData{
				Index: b.Segment, Seq: b.Seq, URI: b.URI, Duration: b.Duration,
				Data: joinBlocks(parts),
			}
			if err := SleepFor(ctx, r.opt.SlowConsumerMs); err != nil && ctx.Err() != nil {
				rep.mu.Lock()
				rep.Cancelled = true
				rep.mu.Unlock()
				break
			}
			r.processOne(ctx, rep, seg)
			parts = parts[:0]
			curBlock = nil
		}
	}()

	wg.Wait()
	rep.Stats = r.queue.Stats()
	rep.ElapsedMs = time.Since(start).Milliseconds()
	if err, ok := firstErr.Load().(error); ok && err != nil {
		return rep, err
	}
	if rep.Fatal != nil {
		return rep, rep.Fatal
	}
	if ctx.Err() != nil || stopProducer.Load() {
		rep.Cancelled = true
	}
	return rep, nil
}

// processOne runs the processor on one segment and commits or quarantines it
// based on the fault kind.
func (r *Runner) processOne(ctx context.Context, rep *Report, seg SegmentData) {
	rep.mu.Lock()
	defer rep.mu.Unlock()
	o := Outcome{Index: seg.Index, Seq: seg.Seq}
	if entry, ok := r.store.Has(seg.Seq); ok {
		o.Skipped = true
		o.Committed = true
		o.PayloadHash = entry.PayloadHash
		o.OutputHash = entry.OutputHash
		o.OutputBytes = entry.Bytes
		rep.Outcomes = append(rep.Outcomes, o)
		rep.ResumedParts++
		return
	}
	res, err := r.proc.Process(ctx, seg)
	if err != nil {
		f, ok := media.AsFault(err)
		if !ok {
			f = media.NewFault(media.FaultStateInvariant, media.SourcePos{Artifact: seg.URI, Segment: seg.Index}, err.Error())
		}
		o.Fault = f.Msg
		o.FaultKind = f.Kind.String()
		o.SourceOffset = f.Pos.Offset
		if f.Pos.Offset < 0 {
			// An unknown source position is reported as 0 rather than
			// omitted, so the JSON line always carries the field.
			o.SourceOffset = 0
		}
		if f.Recoverable() {
			o.Quarantined = true
			_ = r.store.Quarantine(o)
			rep.Outcomes = append(rep.Outcomes, o)
			return
		}
		rep.Fatal = err
		rep.Outcomes = append(rep.Outcomes, o)
		return
	}
	o.Committed = true
	o.PayloadHash = res.PayloadHash
	o.OutputHash = res.OutputHash
	o.Frames = res.Frames
	o.Keyframes = res.Keyframes
	o.AudioSamples = res.AudioSamples
	o.OutputBytes = int64(len(res.Output))
	if err := r.store.CommitOutcome(o, res.Output); err != nil {
		o.Committed = false
		rep.Fatal = err
		rep.Outcomes = append(rep.Outcomes, o)
		return
	}
	rep.Outcomes = append(rep.Outcomes, o)
}

// recordFault attaches a source read failure to the report.
func (r *Runner) recordFault(rep *Report, index int, seq int64, uri string, off int64, err error) {
	rep.mu.Lock()
	defer rep.mu.Unlock()
	o := Outcome{Index: index, Seq: seq, Quarantined: true, SourceOffset: off}
	f, ok := media.AsFault(err)
	if ok {
		o.Fault = f.Msg
		o.FaultKind = f.Kind.String()
	} else {
		o.Fault = err.Error()
		o.FaultKind = media.FaultUnknown.String()
	}
	rep.Outcomes = append(rep.Outcomes, o)
}

func joinBlocks(parts [][]byte) []byte {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
