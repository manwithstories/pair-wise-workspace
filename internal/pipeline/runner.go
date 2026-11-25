// Package pipeline orchestrates the analyzers: it schedules sample-frame
// chunks, runs independent tracks concurrently while preserving deterministic
// event order, commits only verified chunks to the index, and supports
// checkpoint-driven interruption and exact resume.
package pipeline

import (
	"context"
	"errors"
	"sync"

	"mediaaudit/internal/index"
	"mediaaudit/internal/model"
)

// Limits bound memory and input size regardless of malformed metadata.
const (
	MaxTracks      = 32
	MaxChunkFrames = 1 << 20 // ~1M frames per chunk
	MaxTotalFrames = 1 << 34 // ~17G frames absolute guard
)

// Params configures a run. InterruptAfterCheckpoint > 0 requests a deliberate
// stop once that many checkpoints have been written.
type Params struct {
	ChunkFrames           int64
	CheckpointEvery       int64
	InterruptAfterCheckpoint int64
	Workers               int
}

// ChunkFunc is the per-chunk verification callback. It receives the chunk
// sequence and half-open frame range and returns the number of located faults
// in that chunk. Implementations must be safe for concurrent use.
type ChunkFunc func(seq int64, r model.FrameRange) (damage int, err error)

// Runner drives chunk processing against an index file.
type Runner struct {
	idx    *index.File
	params Params
}

// NewRunner validates parameters before any allocation and wraps the index.
func NewRunner(idx *index.File, p Params) (*Runner, error) {
	if p.ChunkFrames <= 0 || p.ChunkFrames > MaxChunkFrames {
		return nil, model.NewFault(model.KindInvalid, "", model.FrameRange{}, -1,
			"chunk-frames out of allowed range")
	}
	if p.CheckpointEvery <= 0 {
		return nil, model.NewFault(model.KindInvalid, "", model.FrameRange{}, -1,
			"checkpoint-every must be positive")
	}
	if p.Workers <= 0 {
		p.Workers = 1
	}
	if p.Workers > MaxTracks {
		p.Workers = MaxTracks
	}
	return &Runner{idx: idx, params: p}, nil
}

// chunkCount derives the total number of chunks covering totalFrames.
func chunkCount(totalFrames, chunkFrames int64) int64 {
	return (totalFrames + chunkFrames - 1) / chunkFrames
}

// Run processes all not-yet-committed chunks in checkpoint-sized batches. Each
// batch verifies up to Workers chunks concurrently, then commits them strictly
// in sequence order so the checkpoint cursor only advances over completed,
// in-order work. An interruption budget stops scheduling new batches, so a
// resumed run touches only chunks never committed.
func (rn *Runner) Run(ctx context.Context, totalFrames int64, fn ChunkFunc) error {
	if totalFrames < 0 || totalFrames > MaxTotalFrames {
		return model.NewFault(model.KindInvalid, "", model.FrameRange{}, -1,
			"total frame count out of allowed range")
	}
	total := chunkCount(totalFrames, rn.params.ChunkFrames)
	seq := rn.idx.NextSeq()
	checkpoints := rn.idx.Payload.CheckpointNo

	for seq < total {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Determine how many chunks this batch may launch.
		batch := rn.params.CheckpointEvery
		remaining := total - seq
		if batch > remaining {
			batch = remaining
		}
		// Respect the interruption budget: never exceed it within a run.
		if rn.params.InterruptAfterCheckpoint > 0 {
			budget := rn.params.InterruptAfterCheckpoint - checkpoints
			if budget <= 0 {
				return interruptedFault(seq, totalFrames)
			}
			if batch > budget {
				batch = budget
			}
		}

		type result struct {
			seq    int64
			r      model.FrameRange
			damage int
			err    error
		}
		results := make([]result, batch)
		sem := make(chan struct{}, rn.params.Workers)
		var wg sync.WaitGroup
		for k := int64(0); k < batch; k++ {
			curSeq := seq + k
			r := frameRange(curSeq, totalFrames, rn.params.ChunkFrames)
			sem <- struct{}{}
			wg.Add(1)
			go func(k, curSeq int64, r model.FrameRange) {
				defer wg.Done()
				defer func() { <-sem }()
				dmg, err := fn(curSeq, r)
				results[k] = result{seq: curSeq, r: r, damage: dmg, err: err}
			}(k, curSeq, r)
		}
		wg.Wait()

		// Commit strictly in order; a failed chunk aborts before advancing.
		for k := int64(0); k < batch; k++ {
			res := results[k]
			if res.err != nil {
				return res.err
			}
			status := "ok"
			if res.damage > 0 {
				status = "damaged"
			}
			rec := index.ChunkRecord{
				Seq: res.seq, FirstFrame: int64(res.r.First),
				LastFrame: int64(res.r.Last), Status: status, DamageCount: res.damage,
			}
			if err := rn.idx.AppendChunk(rec); err != nil {
				return err
			}
			checkpoints++
		}
		seq += batch

		// Reached the interruption checkpoint exactly.
		if rn.params.InterruptAfterCheckpoint > 0 &&
			checkpoints >= rn.params.InterruptAfterCheckpoint && seq < total {
			return interruptedFault(seq-1, totalFrames)
		}
	}
	return rn.idx.MarkFinalized()
}

func interruptedFault(seq, totalFrames int64) error {
	r := frameRange(seq, totalFrames, 1)
	return model.NewFault(model.KindInterrupted, "", r, -1,
		"interrupted after requested checkpoint")
}

// frameRange computes the half-open frame interval for one chunk.
func frameRange(seq, totalFrames, chunkFrames int64) model.FrameRange {
	first := seq * chunkFrames
	last := first + chunkFrames
	if last > totalFrames {
		last = totalFrames
	}
	return model.FrameRange{First: model.Frame(first), Last: model.Frame(last)}
}

// IsInterrupted reports whether an error is the deliberate checkpoint stop.
func IsInterrupted(err error) bool { return errors.Is(err, model.ErrInterrupted) }
