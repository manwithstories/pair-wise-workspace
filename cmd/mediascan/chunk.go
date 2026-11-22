package main

import (
	"fmt"
	"os"

	"mediascan/internal/batch"
	"mediascan/internal/clock"
	"mediascan/internal/gen"
	"mediascan/internal/index"
	"mediascan/internal/quality"
)

func stderr() *os.File { return os.Stderr }

// commitChunks drives the chunked index and checkpoint. It enforces the
// resume / rebuild / interrupt semantics and returns the terminal code.
func commitChunks(o Options, clk clock.Clock, g []gen.Generated, qs *quality.Stats,
	outcomes []quality.FrameOutcome, rep *Report) int {
	inDigest, paramsDigest := digestInput(o)
	rep.InputDigest = inDigest

	store, err := index.Open(o.Out, inDigest)
	if err != nil {
		fmt.Fprintln(stderr(), "index open:", err)
		return exitIndexFault
	}
	if o.RebuildIndex {
		if err := store.Reset(); err != nil {
			fmt.Fprintln(stderr(), "index reset:", err)
			return exitIndexFault
		}
	}
	total := o.totalChunks()
	// On a rebuild the on-disk index was just truncated: start a fresh
	// checkpoint as well rather than verifying against the stale one.
	resume := o.Resume && !o.RebuildIndex
	mgr, err := batch.NewManager(o.Out, inDigest, paramsDigest, total, o.ChunkFrames, resume)
	if err != nil {
		fmt.Fprintln(stderr(), "checkpoint:", err)
		return exitIndexFault
	}
	// index/checkpoint cross-consistency before doing any work
	if resume {
		committed := map[int]bool{}
		for _, e := range store.Summary().Entries {
			committed[e.Chunk] = true
		}
		if err := mgr.VerifyAgainst(committed); err != nil {
			fmt.Fprintln(stderr(), "verify:", err)
			return exitIndexFault
		}
	}

	interrupted := false
	for chunk := 0; chunk < total; chunk++ {
		if store.Committed(chunk) {
			rep.RecoveredChunks++ // already persisted across a resume
			continue
		}
		from := chunk * o.ChunkFrames
		to := from + o.ChunkFrames - 1
		if to >= o.Frames {
			to = o.Frames - 1
		}
		entry := index.Entry{
			Chunk: chunk, FrameFrom: from, FrameTo: to,
			StartTick:   int64(from) * clk.TicksPerFrame(),
			EndTick:     int64(to+1) * clk.TicksPerFrame(),
			TotalFrames: to - from + 1,
		}
		for i := from; i <= to; i++ {
			if outcomes[i].ChecksumOK && outcomes[i].Reachable && !outcomes[i].Lost {
				entry.ValidFrames++
			}
		}
		entry.CorruptRanges = rangesTouching(qs, entry.StartTick, entry.EndTick)
		// validation failures never advance the checkpoint: the index row
		// is written first, and only a successful MarkChunked moves the
		// checkpoint; on error below we return before any further progress.
		if err := store.Commit(entry); err != nil {
			fmt.Fprintln(stderr(), "index commit:", err)
			return exitIndexFault
		}
		if err := mgr.MarkChunked(chunk, entry.StartTick, entry.EndTick); err != nil {
			fmt.Fprintln(stderr(), "checkpoint advance:", err)
			return exitIndexFault
		}
		// simulate an interrupt after the requested number of NEW chunks
		if o.InterruptAfter >= 0 && chunk == o.InterruptAfter-1 && !mgr.Done() {
			interrupted = true
			break
		}
	}

	finalSum := store.Summary()
	rep.CheckpointChunks = finalSum.Chunks
	rep.FirstTick = finalSum.FirstTick
	rep.LastTick = finalSum.LastTick

	committed := map[int]bool{}
	for _, e := range finalSum.Entries {
		committed[e.Chunk] = true
	}
	if err := mgr.VerifyAgainst(committed); err != nil {
		fmt.Fprintln(stderr(), "final verify:", err)
		return exitIndexFault
	}
	switch {
	case interrupted:
		return exitInterrupted
	case rep.FatalFaults > 0:
		return exitCorruptInput
	default:
		return exitOK
	}
}

func rangesTouching(qs *quality.Stats, start, end int64) int {
	n := 0
	for _, iv := range qs.CorruptRanges {
		if iv.Start < end && start < iv.End {
			n++
		}
	}
	return n
}
