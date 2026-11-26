package stream

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"relay/internal/ingest"
	"relay/internal/media"
	"relay/internal/timeline"
)

// Sink consumes one repaired fragment: it writes the fragment output and
// returns the output's byte length and content hash. Implementations must be
// safe to call sequentially from the pipeline's sink goroutine.
type Sink interface {
	Write(rf *timeline.RepairedFragment, outDir string) (size int64, hash string, err error)
}

// Config controls a pipeline run.
type Config struct {
	OutDir       string
	QueueBytes   int64
	ConsumerDelay time.Duration // artificial slow-consumer latency per item
	// InterruptAfter, when > 0, cancels the run after this many commits to
	// simulate an interrupted batch.
	InterruptAfter int
}

// Counters aggregates pipeline outcome counters.
type Counters struct {
	Generated int
	Accepted  int
	Deduped   int
	Isolated  int
	Committed int
	Skipped   int // resumed fragments found in the checkpoint
}

// Pipeline wires ingest -> repair -> bounded sink with back pressure.
type Pipeline struct {
	cfg   Config
	cp    *Checkpoint
	sink  Sink
	queues []*BoundedQueue
}

// New constructs a pipeline and opens (or creates) its checkpoint log.
func New(cfg Config, sink Sink) (*Pipeline, error) {
	if cfg.QueueBytes <= 0 {
		return nil, media.NewFault(media.FaultQueueLimit, 0, 0, 0,
			"queue-bytes must be positive")
	}
	if err := os.MkdirAll(filepath.Join(cfg.OutDir, "frags"), 0o755); err != nil {
		return nil, media.NewFault(media.FaultCheckpoint, 0, 0, 0,
			"create output dir: "+err.Error())
	}
	cp, err := OpenCheckpoint(cfg.OutDir)
	if err != nil {
		return nil, err
	}
	return &Pipeline{
		cfg:    cfg,
		cp:     cp,
		sink:   sink,
		queues: []*BoundedQueue{NewBoundedQueue(cfg.QueueBytes)},
	}, nil
}

// ErrInterrupted is returned when a run stops itself after InterruptAfter
// commits. It is an expected control signal, not a verification failure.
var ErrInterrupted = errors.New("pipeline interrupted after configured commits")

// Run processes repaired fragments in deterministic topo+sequence order.
// On resume, fragments already present in the checkpoint with a matching
// source hash are skipped; the outputs of every run stay byte-identical.
func (p *Pipeline) Run(ctx context.Context, reports []*timeline.TrackReport) (*Counters, error) {
	c := &Counters{}
	q := p.queues[0]

	// Producer: feed accepted fragments in the fixed deterministic order.
	prodErr := make(chan error, 1)
	go func() {
		for _, rep := range reports {
			c.Generated += len(rep.Accepted) + rep.Deduped + len(rep.Isolated)
			c.Deduped += rep.Deduped
			c.Isolated += len(rep.Isolated)
			for _, rf := range rep.Accepted {
				it := Item{
					Key:    rf.Key(),
					Weight: int64(len(rf.Raw)),
					Value:  rf,
				}
				if err := q.Enqueue(ctx, it); err != nil {
					prodErr <- err
					return
				}
			}
		}
		q.Close()
		prodErr <- nil
	}()

	interrupt := p.cfg.InterruptAfter
	// Consumer: bounded sink with slow-consumer delay and checkpoint commits.
	for {
		it, ok, err := q.Dequeue(ctx)
		if err != nil {
			return c, err
		}
		if !ok {
			break
		}
		rf := it.Value.(*timeline.RepairedFragment)
		key := it.Key

		if prior, ok := p.cp.Committed(key); ok && prior.SourceHash == rf.RawHash {
			c.Skipped++
			continue
		}
		if p.cfg.ConsumerDelay > 0 {
			select {
			case <-time.After(p.cfg.ConsumerDelay):
			case <-ctx.Done():
				return c, ctx.Err()
			}
		}
		size, outHash, err := p.sink.Write(rf, p.cfg.OutDir)
		if err != nil {
			return c, err
		}
		entry := CheckpointEntry{
			Key: key, TrackID: rf.TrackID, Sequence: rf.NewSequence,
			SourceHash: rf.RawHash, OutputHash: outHash, OutputSize: size,
		}
		if err := p.cp.Commit(entry); err != nil {
			return c, err
		}
		c.Accepted++
		c.Committed++
		if interrupt > 0 && c.Committed >= interrupt {
			return c, ErrInterrupted
		}
	}
	if err := <-prodErr; err != nil {
		return c, err
	}
	return c, nil
}

// Checkpoint exposes the commit log (used by verification and resume).
func (p *Pipeline) Checkpoint() *Checkpoint { return p.cp }

// PeakQueueBytes reports the maximum buffered bytes across pipeline queues.
func (p *Pipeline) PeakQueueBytes() int64 {
	var peak int64
	for _, q := range p.queues {
		if v := q.Peak(); v > peak {
			peak = v
		}
	}
	return peak
}

// Close releases checkpoint resources.
func (p *Pipeline) Close() error { return p.cp.Close() }

// ParseFaults extracts isolated fragment faults from ingest events.
func ParseFaults(evs []*ingest.ReadEvent) []*media.Fault {
	out := make([]*media.Fault, 0)
	for _, ev := range evs {
		if ev.Fault != nil {
			out = append(out, ev.Fault)
		}
	}
	return out
}
