package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"mediq/internal/checkpoint"
	"mediq/internal/index"
	"mediq/internal/model"
	"mediq/internal/source"
	"mediq/internal/timeline"
	"mediq/internal/validate"
)

// job holds all mutable per-run state. Every field is touched only by the
// consumer goroutine; the producer goroutine communicates through the queue
// and its own counters, which it publishes atomically.
type job struct {
	runner   *Runner
	stats    *Stats
	idx      *index.Writer
	repairer *timeline.Repairer
	started  time.Time

	chunkDigest [32]byte
	indexed     uint64
	chunkHead   checkpoint.ChunkRecord
	readOffset  int64 // resume offset taken from the checkpoint
	committed   int64 // first input byte not yet in a committed chunk
	// replayLimit is the checkpoint's committed offset. Records starting
	// before it were already counted and diagnosed, so a resumed run replays
	// them into the repairer without double-counting their statistics.
	replayLimit int64
	// scanned/scannedCRC track the furthest input byte read and the CRC of
	// the prefix ending there. Records are consumed strictly in order, so this
	// is the digest of the whole scanned prefix, computed without rereading.
	scanned    int64
	scannedCRC uint32
	chunkCount uint64
	// structuralFault is set when record framing itself was lost, which makes
	// publishing a partial index misrepresent the input.
	structuralFault *model.Fault
	sourceDone      bool
}

// prepareCheckpoint loads and validates any resumable checkpoint. The input
// path, size and committed-prefix checksum must all match.
func (r *Runner) prepareCheckpoint(inputSize int64) (*checkpoint.State, error) {
	st, err := r.cks.Load()
	if err != nil {
		if !r.opts.Restart {
			return nil, err
		}
		r.log("discarding unreadable checkpoint: %v", err)
		if err := r.cks.Reset(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if st == nil {
		return nil, nil
	}
	if r.opts.Restart {
		r.log("restart requested: discarding checkpoint generation %d", st.Generation)
		if err := r.cks.Reset(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if st.InputPath != "" && st.InputPath != r.opts.Input {
		return nil, fmt.Errorf("%w: checkpoint belongs to %s", model.ErrCheckpointMismatch, st.InputPath)
	}
	if st.InputSize != inputSize {
		return nil, fmt.Errorf("%w: input size %d != checkpoint %d", model.ErrCheckpointMismatch, inputSize, st.InputSize)
	}
	if st.SourceDone {
		return nil, fmt.Errorf("%w: job already completed at generation %d", model.ErrCheckpointMismatch, st.Generation)
	}
	crc, err := prefixCRC(r.opts.Input, st.CommittedOffset)
	if err != nil {
		return nil, err
	}
	if crc != st.InputCRC {
		return nil, fmt.Errorf("%w: input prefix checksum %08x != checkpoint %08x", model.ErrCheckpointMismatch, crc, st.InputCRC)
	}
	if err := r.chunkLog.Verify(st.ChunkDigest, st.Index.Chunks); err != nil {
		return nil, fmt.Errorf("%w: %v", model.ErrCheckpointMismatch, err)
	}
	r.log("resuming at byte %d (generation %d, %d committed chunks)", st.CommittedOffset, st.Generation, st.Index.Chunks)
	return st, nil
}

func prefixCRC(path string, n int64) (uint32, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return source.NewReader(f, source.Options{}).PrefixCRC(n)
}

// openIndex creates a fresh index or resumes the part file from a checkpoint.
func (r *Runner) openIndex(st *checkpoint.State) (*index.Writer, error) {
	tblDir := r.opts.Output + ".tables"
	if st == nil {
		// A fresh run must not inherit a chunk log from an unrelated job.
		if err := r.chunkLog.Reset(); err != nil {
			return nil, err
		}
		if err := os.Remove(r.opts.Output + ".part"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err := os.RemoveAll(tblDir); err != nil {
			return nil, err
		}
		if err := os.Remove(r.opts.Output); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return index.Create(r.opts.Output, tblDir, r.opts.ChunkFrames)
	}
	w, err := index.Resume(r.opts.Output, tblDir, r.opts.ChunkFrames, st.Index)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", model.ErrCheckpointMismatch, err)
	}
	return w, nil
}

// consume pulls records from the queue, validates and repairs them, writes
// samples in chunks and publishes a checkpoint after every chunk.
func (j *job) consume(ctx context.Context, queue <-chan source.Record) error {
	var chunk []*model.Frame
	for {
		select {
		case rec, ok := <-queue:
			if !ok {
				// The producer finished. Flush the reorder buffer, then the
				// final partial chunk.
				for _, ev := range j.repairer.Flush() {
					if err := j.handleEvent(ctx, ev, &chunk); err != nil {
						return err
					}
				}
				if err := j.flushChunk(&chunk, true); err != nil {
					return err
				}
				return nil
			}
			if err := j.handleRecord(ctx, rec, &chunk); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (j *job) handleRecord(ctx context.Context, rec source.Record, chunk *[]*model.Frame) error {
	if rec.Fault != nil {
		if !(j.stats.Resumed && rec.Fault.Offset < j.replayLimit) {
			j.noteFault(rec.Fault)
		}
		if rec.Fault.HasSeq {
			j.repairer.Skip(rec.Fault.Seq)
		}
		// A fault still consumes bytes, so the prefix digest advances; keep
		// the committed prefix in sync with what was actually scanned.
		if rec.EndOffset > j.scanned {
			j.scanned = rec.EndOffset
			j.scannedCRC = rec.CRC
		}
		if !rec.Fault.Resync() && j.structuralFault == nil {
			// Framing is lost: no later byte can be reliably interpreted, so
			// the index cannot honestly represent the rest of the input.
			j.structuralFault = rec.Fault
		}
		return nil
	}
	fr := rec.Frame
	j.committed = fr.EndOffset
	if fr.EndOffset > j.scanned {
		j.scanned = fr.EndOffset
		j.scannedCRC = rec.CRC
	}
	replay := j.stats.Resumed && fr.StartOffset < j.replayLimit
	if !replay {
		j.stats.FramesRead++
	}
	if fault := validate.CheckFrame(fr); fault != nil {
		if !replay {
			j.noteFault(fault)
		}
		j.repairer.Skip(fr.Seq)
		return nil
	}
	for _, ev := range j.repairer.Push(fr) {
		if err := j.handleEvent(ctx, ev, chunk); err != nil {
			return err
		}
	}
	return nil
}

// handleEvent records a repair decision and queues indexed frames.
func (j *job) handleEvent(ctx context.Context, ev timeline.Event, chunk *[]*model.Frame) error {
	if ev.Status == timeline.StatusQuarantined {
		j.stats.FramesQuarantined++
		if ev.Fault != nil {
			j.noteFault(ev.Fault)
		}
		return nil
	}
	*chunk = append(*chunk, ev.Frame)
	if len(*chunk) < j.runner.opts.ChunkFrames {
		return nil
	}
	if err := j.flushChunk(chunk, false); err != nil {
		return err
	}
	*chunk = (*chunk)[:0]
	return nil
}

// flushChunk writes pending samples to the index, closes the chunk and
// publishes a checkpoint. The final call marks the job complete.
func (j *job) flushChunk(chunk *[]*model.Frame, final bool) error {
	if len(*chunk) > 0 {
		for _, fr := range *chunk {
			if err := j.idx.Append(fr.Seq, fr.MediaTime, fr.Payload); err != nil {
				return fmt.Errorf("index frame %d: %w", fr.Seq, err)
			}
			j.indexed++
		}
		j.stats.FramesIndexed = j.indexed
		*chunk = (*chunk)[:0]
	}
	sum, err := j.idx.CloseChunk()
	if err != nil {
		return err
	}
	idxState, err := j.idx.Commit()
	if err != nil {
		return err
	}
	if sum != nil {
		// Verify the committed bytes really hash to the chunk checksum before
		// advertising this chunk as recoverable.
		if err := j.idx.VerifyLastChunk(*sum, idxState); err != nil {
			return err
		}
		rec := checkpoint.ChunkRecord{
			Index:      sum.Index,
			FirstFrame: sum.FirstFrame,
			LastFrame:  sum.LastFrame,
			Samples:    sum.Samples,
			Checksum:   sum.Checksum,
		}
		digest, err := j.runner.chunkLog.Append(rec, j.chunkDigest)
		if err != nil {
			return err
		}
		j.chunkDigest = digest
		j.chunkHead = rec
		j.chunkCount++
		j.stats.CommittedChunks = j.chunkCount
	}
	if final {
		j.sourceDone = true
	}
	return j.writeCheckpoint(idxState, final)
}

func (j *job) writeCheckpoint(idxState index.State, done bool) error {
	// Resume must re-read every record from the earliest frame still buffered
	// by the reorder window, because those frames are not in the index yet.
	// Anything before that is fully accounted for: indexed, quarantined, or
	// diagnosed as corrupt.
	offset := j.committed
	if floor, ok := j.repairer.PendingFloor(); ok && floor < offset {
		offset = floor
	}
	if done {
		offset = j.stats.InputSize
	}
	// Snap the prefix digest at the chosen resume offset. If the offset sits
	// after the last scanned record (the normal case), the running CRC already
	// covers exactly that prefix.
	crc := j.scannedCRC
	if offset != j.scanned {
		var err error
		crc, err = prefixCRC(j.runner.opts.Input, offset)
		if err != nil {
			return err
		}
	}
	st := &checkpoint.State{
		InputPath:         j.runner.opts.Input,
		InputSize:         j.stats.InputSize,
		InputCRC:          crc,
		CommittedOffset:   offset,
		SourceDone:        done,
		ChunkHead:         j.chunkHead,
		ChunkDigest:       j.chunkDigest,
		Index:             idxState,
		Timeline:          j.repairer.Snapshot(),
		FramesRead:        j.stats.FramesRead,
		FramesBad:         j.stats.FramesBad,
		FramesQuarantined: j.stats.FramesQuarantined,
	}
	return j.runner.cks.Save(st)
}

// noteFault records a diagnostic and the first corrupt frame for the report.
func (j *job) noteFault(f *model.Fault) {
	if f.Code == model.CodeHashMismatch || f.Code == model.CodeTruncatedPayload ||
		f.Code == model.CodeTruncatedHeader || f.Code == model.CodeMetadataMismatch ||
		f.Code == model.CodeOversizeFrame || f.Code == model.CodeOversizeMetadata {
		j.stats.FramesBad++
	}
	switch f.Code {
	case model.CodeQuarantined, model.CodeDuplicateSeq, model.CodeOutOfOrder,
		model.CodeUnrepairableTime, model.CodeTimestampWrap, model.CodeBadTimebase:
		j.stats.QuarantineReasons[f.Code]++
	}
	if !j.stats.HasCorrupt {
		switch f.Code {
		case model.CodeHashMismatch, model.CodeTruncatedHeader, model.CodeTruncatedPayload,
			model.CodeMetadataMismatch, model.CodeOversizeFrame:
			j.stats.HasCorrupt = true
			j.stats.FirstCorruptFrame = f.Seq
			j.stats.FirstCorruptOffset = f.Offset
		}
	}
	j.runner.log("fault: %s", f.Error())
}

// finishOnDisk turns the part file into the final MP4, verifies it against
// the source, and publishes it only when the index is valid.
func (j *job) finishOnDisk(idx *index.Writer) error {
	if err := idx.Finish(); err != nil {
		return err
	}
	if err := idx.Publish(); err != nil {
		return err
	}
	rep, err := index.VerifyFile(j.runner.opts.Output, index.VerifyOptions{})
	if err != nil {
		return err
	}
	j.stats.OutputSHA256 = rep.OutputSHA256
	j.stats.IndexValid = rep.IndexValid
	if !rep.IndexValid {
		// A published-but-invalid index must not be left behind as if it were
		// a deliverable.
		if rmErr := os.Remove(j.runner.opts.Output); rmErr != nil {
			j.runner.log("warning: could not remove invalid output: %v", rmErr)
		}
		if err := j.runner.cks.Reset(); err != nil {
			j.runner.log("warning: could not reset checkpoint: %v", err)
		}
		return fmt.Errorf("%w: %v", model.ErrVerifyFailed, rep.ProblemsAsError())
	}
	return nil
}

var _ = io.Discard
