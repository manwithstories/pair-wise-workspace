package pipeline

import (
	"context"
	"time"

	"mediapack/internal/checkpoint"
	"mediapack/internal/clock"
	"mediapack/internal/manifest"
	"mediapack/internal/stream"
	"mediapack/internal/timeline"
)

// Checkpointer is the subset of checkpoint.Store the pipeline needs,
// kept as an interface so recovery tests can use fakes.
type Checkpointer interface {
	Save(checkpoint.State) error
	Verify(st checkpoint.State, sourceFP, manifestSHA string) error
	Exists() bool
}

// ResumeState is the validated record handed back into Run.
type ResumeState struct {
	ParseOffset int64
	WatermarkNS int64
	ManifestSHA string
	SourceFP    string
}

func (r ResumeState) toCheckpoint() checkpoint.State {
	return checkpoint.State{
		SourceFP: r.SourceFP, ParseOffset: r.ParseOffset,
		WatermarkNS: r.WatermarkNS, ManifestSHA: r.ManifestSHA,
	}
}

// wrapTracker resolves Matroska uint16 block timecode wrap onto a
// monotonic tick axis using the clock's RTP-style helpers.
type wrapTracker struct {
	havePrev bool
	prevRaw  int64
	prevUW   int64
}

func newWrapTracker() *wrapTracker { return &wrapTracker{} }

// resolve lifts a uint16 modular timecode to a monotonic value.
func (w *wrapTracker) resolve(raw uint16) int64 {
	if !w.havePrev {
		w.havePrev = true
		w.prevRaw, w.prevUW = int64(raw), int64(raw)
		return int64(raw)
	}
	uw := clock.Unwrap16(w.prevRaw, w.prevUW, raw)
	w.prevRaw, w.prevUW = int64(raw), uw
	return uw
}

type runner struct {
	cfg          Config
	tb           *clock.Timebase
	engine       *timeline.Engine
	queue        *stream.BoundedQueue
	out          *Outputs
	cp           Checkpointer
	fp           string
	wrap         *wrapTracker
	metrics      Metrics
	resumed      bool
	resumeOffset int64
	events       []manifest.InputEvent
	interrupted  bool
}

// feed runs the streaming parser in a producer goroutine (blocking on
// the bounded queue under back-pressure) while the main goroutine
// drains items into the manifest event set, so queue occupancy reflects
// real producer/consumer speed differences rather than always being zero.
func (r *runner) feed(ctx context.Context, data []byte) (ParseSummary, error) {
	cfg := ParseConfig{MaxElementBytes: r.cfg.MaxElement, MaxBlockBytes: r.cfg.MaxBlock}
	type feedErr struct {
		sum ParseSummary
		err error
	}
	done := make(chan feedErr, 1)

	go func() {
		// Closing on exit unblocks the consumer's Dequeue so it can
		// observe the producer error through the done channel.
		defer r.queue.Close()
		summary, err := parseMatroska(bytesReader(data), cfg, func(ev BlockEvent) error {
			// On resume we still replay every block from the start:
			// deterministic replay is what makes the regenerated
			// manifest byte-identical to a clean run. The checkpoint
			// offset is verified separately rather than used to skip.
			rawTC := uint16(ev.AbsTicks)
			monoTicks := r.wrap.resolve(rawTC)
			timeNS, err := r.tb.ToNanos(monoTicks)
			if err != nil {
				return err
			}
			te := timeline.Event{
				Track: ev.Track, TimeNS: timeNS, RawTime: ev.AbsTicks,
				ByteOff: ev.HeaderOff, PayloadLen: ev.PayloadLen, Keyframe: ev.Keyframe,
			}
			res := r.engine.Push(te)
			if res.Rejected {
				return clock.AtError(res.Reason, ev.HeaderOff, "event outside replay window")
			}
			item := stream.Item{
				Track: ev.Track, TimeNS: timeNS, ByteOff: ev.HeaderOff,
				Payload: ev.Payload,
			}
			// Blocks here when the byte budget is exhausted: this is
			// the back-pressure edge that pauses the parser.
			if err := r.queue.Enqueue(ctx, item); err != nil {
				return err
			}
			if r.cfg.InterruptAfterSegments > 0 &&
				r.engine.CommittedCount() >= r.cfg.InterruptAfterSegments {
				r.interrupted = true
				return errInterrupted
			}
			return nil
		})
		done <- feedErr{summary, err}
	}()

	// Consumer: poll the queue and watch for producer completion. It
	// yields the CPU briefly when empty so a blocked producer can make
	// progress and the bounded-queue peak stays meaningful.
	for {
		if ctx.Err() != nil {
			return ParseSummary{}, clock.NewError(clock.CatCancelled, "%v", ctx.Err())
		}
		if it, ok := r.queue.TryDequeue(); ok {
			r.events = append(r.events, manifest.InputEvent{
				Track: it.Track, TimeNS: it.TimeNS,
				ByteOff: it.ByteOff, PayloadLen: int64(len(it.Payload)),
			})
			continue
		}
		// Empty queue: either the producer finished, or it is blocked
		// waiting for room (which TryDequeue just created).
		select {
		case fe := <-done:
			r.drainRemaining(ctx)
			return fe.sum, fe.err
		case <-time.After(time.Millisecond):
		case <-ctx.Done():
			return ParseSummary{}, clock.NewError(clock.CatCancelled, "%v", ctx.Err())
		}
	}
}

// drainRemaining empties already-queued items after the producer ends.
func (r *runner) drainRemaining(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		it, ok := r.queue.TryDequeue()
		if !ok {
			return
		}
		r.events = append(r.events, manifest.InputEvent{
			Track: it.Track, TimeNS: it.TimeNS,
			ByteOff: it.ByteOff, PayloadLen: int64(len(it.Payload)),
		})
	}
}

// buildManifest aligns accumulated events onto the common grid across
// two representations and computes the deterministic checksum.
func (r *runner) buildManifest() error {
	reps := r.out.Representations()
	spec := manifest.GridSpec{
		GridNS:      r.cfg.GridMS * 1_000_000,
		ToleranceNS: r.cfg.ToleranceMS * 1_000_000,
		Reps:        reps,
	}
	m, err := manifest.Align(r.events, spec, r.fp)
	if err != nil {
		return err
	}
	sum, err := manifest.Checksum(m)
	if err != nil {
		return err
	}
	if err := r.out.WriteManifest(m, sum); err != nil {
		return err
	}
	r.metrics.ManifestSHA256 = sum
	r.metrics.Segments = len(m.Segments)
	r.metrics.Representations = len(reps)
	r.metrics.ManifestErrors = len(m.Errors)
	return nil
}

func (r *runner) checkpoint(endOffset int64) error {
	st := checkpoint.State{
		SourceFP:     r.fp,
		ParseOffset:  endOffset,
		WatermarkNS:  r.engine.Watermark(),
		ManifestSHA:  r.metrics.ManifestSHA256,
		SegmentCount: r.metrics.Segments,
		QueueBytes:   r.queue.Stats().Bytes,
	}
	return r.cp.Save(st)
}

func (r *runner) finalize(start time.Time, runErr error) (*Metrics, error) {
	s := r.queue.Stats()
	r.metrics.PeakBufferBytes = s.PeakBytes
	r.metrics.QueueLimitBytes = s.Limit
	r.metrics.LateEvents = r.engine.LateCount()
	r.metrics.Duplicates = r.engine.DupCount()
	r.metrics.Resumed = r.resumed
	r.metrics.ResumeMS = time.Since(start).Milliseconds()
	if runErr != nil && runErr != errInterrupted {
		return &r.metrics, runErr
	}
	return &r.metrics, nil
}
