package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"mediacheck/internal/fault"
)

type item struct {
	seg  SegmentHash
	data []byte
}

// Run streams r through the stages. align is the caption-alignment
// hook invoked per segment (may be slow; backpressure propagates
// upstream). Checkpoint state at cpPath (if non-empty) is verified and
// extended; a pre-existing checkpoint resumes the run.
func Run(ctx context.Context, r io.Reader, cfg Config, cpPath string,
	align func(*SegmentHash) error) (*Result, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	res := &Result{Faults: &fault.List{}, ResumeMatches: true}
	state, resumed, err := loadCheckpoint(cpPath, cfg)
	if err != nil {
		return nil, err
	}
	res.Resumed = resumed

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var firstErr atomic.Value
	fail := func(err error) {
		firstErr.CompareAndSwap(nil, err)
		cancel()
	}

	sem := make(chan struct{}, cfg.QueueCap) // total in-flight chunks
	var inFlight, peak atomic.Int64
	ch1 := make(chan item, cfg.QueueCap)
	ch2 := make(chan item, cfg.QueueCap)
	ch3 := make(chan item, cfg.QueueCap)
	var wg sync.WaitGroup
	stage := func(f func()) {
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	}
	send := func(ch chan<- item, it item) bool {
		select {
		case ch <- it:
			return true
		case <-ctx.Done():
			return false
		}
	}

	// Stage 1: read bounded chunks.
	stage(func() {
		defer close(ch1)
		off := int64(0)
		for {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			buf := make([]byte, cfg.ChunkSize)
			n, err := io.ReadFull(r, buf)
			if n == 0 {
				<-sem
				return // clean EOF
			}
			buf = buf[:n]
			inFlight.Add(int64(n))
			if v := inFlight.Load(); v > peak.Load() {
				peak.Store(v)
			}
			it := item{seg: SegmentHash{Offset: off, Size: n}, data: buf}
			off += int64(n)
			if !send(ch1, it) {
				inFlight.Add(-int64(n))
				<-sem
				return
			}
			if err == io.ErrUnexpectedEOF {
				return
			}
		}
	})
	// Stage 2: analyze — hash normalized chunk bytes.
	stage(func() {
		defer close(ch2)
		idx := 0
		for it := range ch1 {
			sum := sha256.Sum256(it.data)
			it.seg.Index = idx
			it.seg.SHA256 = hex.EncodeToString(sum[:])
			idx++
			if !send(ch2, it) {
				return
			}
		}
	})
	// Stage 3: caption alignment hook (may be slow → backpressure).
	stage(func() {
		defer close(ch3)
		for it := range ch2 {
			if align != nil {
				if err := align(&it.seg); err != nil {
					fail(fault.New(fault.CatPipeline, it.seg.Offset, false,
						"align stage: %v", err))
					return
				}
			}
			if !send(ch3, it) {
				return
			}
		}
	})
	// Stage 4: summarize, verify/commit checkpoint, release tokens.
	stage(func() {
		for it := range ch3 {
			n := int64(it.seg.Size)
			inFlight.Add(-n)
			<-sem
			res.TotalBytes += n
			if err := state.commit(it.seg, cpPath); err != nil {
				fail(err)
				return
			}
			res.Segments = append(res.Segments, it.seg)
		}
	})
	wg.Wait()

	if e := firstErr.Load(); e != nil {
		return res, e.(error)
	}
	if err := ctx.Err(); err != nil {
		// Canceled: keep the partial checkpoint for resume, but do
		// not mark the run done.
		res.Faults.Addf(fault.CatPipeline, -1, true, "run canceled: %v", err)
		_ = state.save(cpPath)
		res.PeakBufferBytes = peak.Load()
		return res, nil
	}
	// Verify the whole input against a resumed checkpoint.
	if resumed {
		if state.InputSHA256 != "" && state.InputSHA256 != state.inputHash() {
			res.ResumeMatches = false
			return res, fault.New(fault.CatCheckpoint, -1, false,
				"input changed since checkpoint: hash mismatch")
		}
		if !state.verified {
			res.ResumeMatches = false
		}
	}
	state.InputSHA256 = state.inputHash()
	state.Done = true
	if err := state.save(cpPath); err != nil {
		return res, err
	}
	res.PeakBufferBytes = peak.Load()
	return res, nil
}

var _ = fmt.Sprintf
