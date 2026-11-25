// Package runner orchestrates the archive pipeline end to end.
package runner

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	"mediaarchive/internal/checkpoint"
	"mediaarchive/internal/clock"
	"mediaarchive/internal/demux"
	"mediaarchive/internal/model"
	"mediaarchive/internal/pipeline"
	rtpinspect "mediaarchive/internal/rtp"
	"mediaarchive/internal/track"
)

// ErrCanceled marks a clean stop triggered by --stop-after; the CLI maps it
// to exit code 4 and keeps the checkpoint for a later resume.
var ErrCanceled = errors.New("run stopped after configured sample boundary")

// RunOutcome aggregates everything the CLI needs to report and build the
// manifest.
type RunOutcome struct {
	Container  *demux.Container
	Normalized []clock.NormalizedSample
	Checkpoint *checkpoint.State
	InputSHA   string
	Canceled   bool
	Damages    []model.Error
	TotalOrder uint64
}

// Run executes one remux pass (possibly resumed) up to the configured sample
// boundary. Only whole validated chunk boundaries are ever committed.
func Run(ctx context.Context, opts Options) (*RunOutcome, error) {
	inSHA, inSize, err := checkpoint.Fingerprint(opts.Input)
	if err != nil {
		return nil, err
	}
	c, err := demux.Open(opts.Input)
	if err != nil {
		return nil, err
	}
	assignments, err := loadMapping(c, opts.MapFile)
	if err != nil {
		return nil, err
	}
	mapping, err := track.Build(c, assignments)
	if err != nil {
		return nil, err
	}
	normalizer, err := clock.NewNormalizer(c.Tracks, mapping.OrderedTracks)
	if err != nil {
		return nil, err
	}
	state, err := loadState(opts, inSHA, inSize)
	if err != nil {
		return nil, err
	}
	pool, err := pipeline.New(pipeline.Config{
		Workers: opts.Workers, Queue: opts.Queue, MaxBufferedBytes: opts.BufferMem,
	})
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan pipeline.Job, opts.Queue)
	results := pool.Run(runCtx, jobs)

	pr := &producer{
		ctx: runCtx, c: c, mapping: mapping, normalizer: normalizer,
		state: state, pool: pool, jobs: jobs, opts: opts,
		inspectors: map[uint32]*rtpinspect.Inspector{},
	}
	// Producer runs concurrently; the main goroutine drains results so the
	// bounded queue can never fill into a two-sided block.
	prodErr := make(chan error, 1)
	go func() { prodErr <- pr.run() }()

	// Drain concurrently so the bounded queue can always make progress.
	var collected []clock.NormalizedSample
	var derr error
	done := make(chan struct{})
	go func() {
		for r := range results {
			if r.Err != nil && derr == nil {
				derr = r.Err
			}
			collected = append(collected, r.Sample)
		}
		close(done)
	}()
	perr := <-prodErr
	close(jobs) // signal workers to finish after the last enqueued job
	<-done
	if perr != nil && !errors.Is(perr, context.Canceled) {
		return nil, perr
	}
	if derr != nil && !errors.Is(derr, context.Canceled) {
		return nil, derr
	}
	canceled := errors.Is(perr, context.Canceled)
	expected := map[uint32]int{}
	for _, table := range c.Samples {
		for _, s := range table {
			expected[s.ChunkSeq]++
		}
	}
	if err := commitChunks(state, expected, collected, pr.order); err != nil {
		return nil, err
	}
	out := &RunOutcome{
		Container: c, Normalized: collected, Checkpoint: state,
		InputSHA: inSHA, Canceled: canceled, Damages: pr.damages, TotalOrder: pr.order,
	}
	if canceled && opts.Checkpoint != "" {
		if err := state.Save(opts.Checkpoint); err != nil {
			return nil, err
		}
		return out, ErrCanceled
	}
	return out, nil
}

type producer struct {
	ctx        context.Context
	c          *demux.Container
	mapping    *track.Mapping
	normalizer *clock.Normalizer
	state      *checkpoint.State
	pool       *pipeline.Pool
	jobs       chan pipeline.Job
	opts       Options
	inspectors map[uint32]*rtpinspect.Inspector
	damages    []model.Error
	order      uint64
	processed  int
}

func (p *producer) run() error {
	stop := p.opts.StopAfter > 0
	return p.c.StreamSamples(
		demux.StreamOptions{RecoverTruncated: p.opts.OnDamage == "recover"},
		func(raw demux.Sample) error {
			if tm, ok := p.c.Track(raw.TrackID); ok && tm.Kind == demux.KindHint {
				return inspectHint(p.inspectors, raw, &p.damages, p.opts)
			}
			if !p.mapping.IsMapped(raw.TrackID) {
				return model.NewError(model.StageTrack,
					fmt.Sprintf("unmapped sample reached pipeline on track %d", raw.TrackID)).
					WithTrack(int32(raw.TrackID)).WithSample(raw.Seq)
			}
			ns, err := p.normalizer.Apply(raw)
			if err != nil {
				return err
			}
			if stop && p.processed >= p.opts.StopAfter {
				return context.Canceled
			}
			if err := p.pool.Acquire(p.ctx, len(raw.Payload)); err != nil {
				return err
			}
			p.order++
			job := pipeline.Job{Order: p.order, Sample: ns}
			select {
			case p.jobs <- job:
			case <-p.ctx.Done():
				p.pool.Release(len(raw.Payload))
				return p.ctx.Err()
			}
			p.processed++
			return nil
		})
}

func mustHash(hexStr string) [32]byte {
	var out [32]byte
	b, err := hex.DecodeString(hexStr)
	if err != nil || len(b) != 32 {
		panic("checkpoint stored invalid sha256: " + hexStr)
	}
	copy(out[:], b)
	return out
}
