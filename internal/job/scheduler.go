package job

import (
	"context"
	"errors"
	"fmt"
	"log"

	"rcbench/internal/codec"
	"rcbench/internal/quality"
	"rcbench/internal/rate"
	"rcbench/internal/source"
)

// Options tunes scheduler behavior.
type Options struct {
	Workers       int
	MaxRetries    int
	FailAtSegment int // 1-based; 0 disables failure injection
	Logger        *log.Logger
}

// ErrPoisoned is returned when a segment exhausts its retries.
var ErrPoisoned = errors.New("segment poisoned: retries exhausted")

// SegmentOutcome summarizes one encoded segment.
type SegmentOutcome struct {
	Segment int
	QP      int
	Bits    int64
	PSNR    float64
	Skipped bool // true when replayed from a valid checkpoint
}

// frameResult carries one encoded frame back to the assembler.
type frameResult struct {
	offset  int // frame offset inside the segment
	bits    int
	payload []byte
	psnr    float64
	err     error
}

// encodeSegment encodes all frames of [startFrame,endFrame) at qp using a
// frame-level worker pool. Results are assembled in frame order, so the
// output is independent of the worker count.
func encodeSegment(ctx context.Context, rdr *source.Reader, startFrame, endFrame, qp, workers int) (totalBits int64, meanPSNR float64, err error) {
	n := endFrame - startFrame
	jobs := make(chan int)
	results := make(chan frameResult, n)
	if workers < 1 {
		workers = 1
	}
	for w := 0; w < workers; w++ {
		go func() {
			frameBuf := make([]byte, rdr.FrameSize())
			for off := range jobs {
				idx := startFrame + off
				if e := rdr.ReadFrame(idx, frameBuf); e != nil {
					results <- frameResult{offset: off, err: e}
					continue
				}
				payload, bits, e := codec.EncodeFrame(frameBuf, rdrW(rdr), rdrH(rdr), qp)
				if e != nil {
					results <- frameResult{offset: off, err: e}
					continue
				}
				dec, _, e := codec.DecodeFrame(payload, bits, rdrW(rdr), rdrH(rdr))
				if e != nil {
					results <- frameResult{offset: off, err: e}
					continue
				}
				results <- frameResult{offset: off, bits: bits, payload: payload, psnr: quality.FramePSNR(frameBuf, dec)}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for off := 0; off < n; off++ {
			select {
			case <-ctx.Done():
				return
			case jobs <- off:
			}
		}
	}()
	psnrs := make([]float64, n)
	done := 0
	for done < n {
		select {
		case <-ctx.Done():
			return 0, 0, ctx.Err()
		case r := <-results:
			if r.err != nil {
				return 0, 0, r.err
			}
			totalBits += int64(r.bits)
			psnrs[r.offset] = r.psnr
			done++
		}
	}
	mean, _ := quality.MeanStd(psnrs)
	return totalBits, mean, nil
}

func rdrW(r *source.Reader) int { w, _ := r.Geometry(); return w }
func rdrH(r *source.Reader) int { _, h := r.Geometry(); return h }

// Run executes every unfinished segment of the job in segment order.
// Completed segments with matching content hash are replayed into the
// controller so the QP trajectory stays identical across resumes.
func Run(ctx context.Context, jobDir string, meta *Meta, rdr *source.Reader, ctrl *rate.Controller, opts Options) ([]SegmentOutcome, error) {
	if opts.Workers < 1 {
		opts.Workers = 1
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.New(log.Writer(), "", 0)
	}
	outcomes := make([]SegmentOutcome, 0, meta.Segments)
	for seg := 1; seg <= meta.Segments; seg++ {
		if err := ctx.Err(); err != nil {
			return outcomes, err
		}
		start := (seg - 1) * meta.SegmentFrames
		end := start + meta.SegmentFrames
		if end > meta.TotalFrames {
			end = meta.TotalFrames
		}
		hash, err := HashSegmentSource(rdr, start, end, rdr.FrameSize())
		if err != nil {
			return outcomes, fmt.Errorf("segment %d: %w", seg, err)
		}
		if ck, err := ReadCheckpoint(jobDir, seg); err == nil && ck.Hash == hash && ck.Frames == end-start {
			ctrl.Update(ck.QP, ck.Bits)
			outcomes = append(outcomes, SegmentOutcome{Segment: seg, QP: ck.QP, Bits: ck.Bits, PSNR: ck.PSNR, Skipped: true})
			logger.Printf("segment %d/%d: checkpoint valid, skipped (qp=%d bits=%d psnr=%.2f)", seg, meta.Segments, ck.QP, ck.Bits, ck.PSNR)
			continue
		}
		qp := ctrl.DecideQP()
		var bits int64
		var psnr float64
		attempts := 1 + opts.MaxRetries
		var lastErr error
		for attempt := 1; attempt <= attempts; attempt++ {
			if opts.FailAtSegment == seg {
				lastErr = fmt.Errorf("injected failure at segment %d (attempt %d/%d)", seg, attempt, attempts)
			} else {
				bits, psnr, lastErr = encodeSegment(ctx, rdr, start, end, qp, opts.Workers)
			}
			if lastErr == nil {
				break
			}
			logger.Printf("segment %d/%d: attempt %d/%d failed: %v", seg, meta.Segments, attempt, attempts, lastErr)
			if errors.Is(lastErr, context.Canceled) || errors.Is(lastErr, context.DeadlineExceeded) {
				return outcomes, lastErr
			}
		}
		if lastErr != nil {
			return outcomes, fmt.Errorf("segment %d: %w", seg, ErrPoisoned)
		}
		ck := &SegmentCheckpoint{Segment: seg, Frames: end - start, QP: qp, Bits: bits, PSNR: psnr, Hash: hash}
		if err := WriteCheckpoint(jobDir, ck); err != nil {
			return outcomes, fmt.Errorf("segment %d: checkpoint: %w", seg, err)
		}
		under := ctrl.Update(qp, bits)
		outcomes = append(outcomes, SegmentOutcome{Segment: seg, QP: qp, Bits: bits, PSNR: psnr})
		logger.Printf("segment %d/%d: qp=%d bits=%d psnr=%.2f vbv=%d%s", seg, meta.Segments, qp, bits, psnr, ctrl.VBVLevel(), map[bool]string{true: " VBV-UNDERFLOW", false: ""}[under])
	}
	return outcomes, nil
}
