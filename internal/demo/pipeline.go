// Package demo contains the demo adapter: it wires the reusable library
// packages (clock, manifest, ingest, timeline, stream, index) into one
// deterministic offline pipeline. The core libraries never import this
// package, so they remain usable independently.
package demo

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/clearliu/fieldcut/internal/clock"
	"github.com/clearliu/fieldcut/internal/fixtures"
	"github.com/clearliu/fieldcut/internal/index"
	"github.com/clearliu/fieldcut/internal/ingest"
	"github.com/clearliu/fieldcut/internal/stream"
	"github.com/clearliu/fieldcut/internal/timeline"
	"github.com/pion/rtp"
)

// Options is the parsed and validated demo configuration.
type Options struct {
	Seed             int64
	Duration         time.Duration
	QueueBytes       int
	QueueItems       int
	LateWindow       time.Duration
	InjectLoss       float64
	SlowConsumerMs   int
	Case             fixtures.Case
}

// Metrics is the terminal output contract. Every field maps to one
// acceptance criterion in the task.
type Metrics struct {
	AVSyncP95Ms        int
	PacketRecoveryRate float64
	PipelineLatencyP95Ms int64
	PeakBufferBytes    int
	LateReplayCount    int
	IndexSeekMisses    int
	IndexSHA256        string
	Quarantined        int
	UntrustedEvents    int
	ProcessedPackets   int
	GapPackets         int
}

// hardLimits enforces the project's memory/input ceilings regardless of
// what the command line asks for.
func hardLimitOptions(o *Options) error {
	if o.QueueBytes <= 0 || o.QueueBytes > stream.MaxQueueBytes {
		return fmt.Errorf("%w: queue-bytes must be in (0,%d]", clock.ErrInvalidArgument, stream.MaxQueueBytes)
	}
	if o.QueueItems <= 0 || o.QueueItems > stream.MaxQueueItems {
		return fmt.Errorf("%w: queue-items out of range", clock.ErrInvalidArgument)
	}
	if o.InjectLoss < 0 || o.InjectLoss >= 1 {
		return fmt.Errorf("%w: inject-loss must be in [0,1)", clock.ErrInvalidArgument)
	}
	if o.Duration <= 0 {
		return fmt.Errorf("%w: duration must be positive", clock.ErrInvalidArgument)
	}
	if clock.FromDuration(o.LateWindow) > clock.Ticks(clock.MaxLateWindowTicks) {
		return fmt.Errorf("%w: late-window exceeds hard cap", clock.ErrInvalidArgument)
	}
	return nil
}

// Run executes the offline pipeline and returns deterministic metrics.
// It performs no network IO and spawns goroutines only inside the bounded
// stream package, which cancels and drains them on error.
func Run(ctx context.Context, o Options) (Metrics, error) {
	if err := hardLimitOptions(&o); err != nil {
		return Metrics{}, err
	}
	cfg := buildFixtureConfig(o)

	// 1. Generate inputs locally.
	packets, err := fixtures.GeneratePackets(cfg)
	if err != nil {
		return Metrics{}, err
	}
	segments, err := fixtures.GenerateSegments(cfg, 0)
	if err != nil {
		return Metrics{}, err
	}

	// 2. Two independent bounded queue chains: packets flow through the
	// ingest/timeline stage, segments through validate/index. Splitting
	// them keeps payload types homogeneous per queue.
	packetQ := stream.NewBoundedQueue(o.QueueBytes, o.QueueItems)
	segQ := stream.NewBoundedQueue(o.QueueBytes, o.QueueItems)

	wm := timeline.NewWatermark(timeline.DefaultRepairConfig())
	engine := timeline.NewReplayEngine(clock.FromDuration(o.LateWindow))
	reorder := ingest.NewReorderBuffer(ingest.ReorderConfig{WindowSize: 64})
	builder := index.NewBuilder()
	var (
		qMu        sync.Mutex
		quarantine []index.QuarantineRecord
	)
	addQuarantine := func(q index.QuarantineRecord) {
		qMu.Lock()
		quarantine = append(quarantine, q)
		qMu.Unlock()
	}

	// 3. Packet stage: validate, reorder, repair timestamps, route late
	// events to the replay engine. Outputs are nil: all state is applied
	// to the timeline structures owned by this stage.
	stage1 := stream.NewStage(stream.StageConfig{Name: "ingest-timeline"}, packetQ, nil,
		func(_ context.Context, in stream.Item) ([]stream.Item, error) {
			p := in.Payload.(fixtures.Packet)
			pkt, err := ingest.ParsePacket(p.Raw)
			if err != nil {
				return nil, err
			}
			src := ingest.SourceLive
			if p.IsRTX {
				src = ingest.SourceRTX
			}
			ordered, late, err := reorder.Push(pkt, src)
			if err != nil {
				return nil, err
			}
			handleOrdered(wm, engine, ordered)
			for _, l := range late {
				ev := orderedEvent(l.Packet, 0, l.Recovered, l.Source)
				rep := engine.IngestLate(ev)
				if rep.Outcome == timeline.ReplayIsolated {
					addQuarantine(index.QuarantineRecord{
						TrackID: ev.TrackID, Sequence: ev.Sequence, Reason: "late-outside-window",
					})
				}
			}
			return nil, nil
		})

	// 4. Segment stage: validate boxes, build index entries; truncated
	// boxes are quarantined rather than aborting the run. The artificial
	// delay models the slow archive disk and drives latency metrics.
	var syncOff int64
	var offMu sync.Mutex
	stage2 := stream.NewStage(stream.StageConfig{
		Name: "validate-index", ProcessDelay: time.Duration(o.SlowConsumerMs) * time.Millisecond,
	}, segQ, nil,
		func(_ context.Context, in stream.Item) ([]stream.Item, error) {
			seg := in.Payload.(fixtures.Segment)
			if _, err := ingest.ValidateSegment(seg.Data); err != nil {
				addQuarantine(index.QuarantineRecord{
					TrackID: seg.TrackID, Sequence: seg.Sequence, Reason: err.Error(),
				})
				return nil, nil
			}
			offMu.Lock()
			err := builder.Add(index.Entry{
				TrackID: seg.TrackID, Sequence: seg.Sequence,
				Start: seg.Start, End: seg.Start.Add(seg.Duration),
				Offset: syncOff, Size: int64(len(seg.Data)),
			})
			syncOff += int64(len(seg.Data))
			offMu.Unlock()
			return nil, err
		})

	// 5. Feed both queues then close; Pipeline drains and cancels on error.
	feedErr := make(chan error, 2)
	go func() {
		for _, p := range packets {
			if err := packetQ.Enqueue(ctx, stream.Item{
				TrackID: 0, Sequence: int(p.Seq), Size: len(p.Raw), Payload: p,
			}); err != nil {
				feedErr <- err
				return
			}
		}
		packetQ.Close()
		feedErr <- nil
	}()
	go func() {
		for _, s := range segments {
			if err := segQ.Enqueue(ctx, stream.Item{
				TrackID: s.TrackID, Sequence: s.Sequence, Size: len(s.Data), Payload: s,
			}); err != nil {
				feedErr <- err
				return
			}
		}
		segQ.Close()
		feedErr <- nil
	}()
	if err := stream.Pipeline(ctx, []*stream.Stage{stage1, stage2}, packetQ, segQ); err != nil {
		return Metrics{}, err
	}
	for i := 0; i < 2; i++ {
		if err := <-feedErr; err != nil {
			return Metrics{}, err
		}
	}
	// Pipeline has joined both worker goroutines, so touching their
	// state here is race-free (wg.Wait establishes happens-before).
	// Flush any packets still held by the reorder buffer at stream end.
	handleOrdered(wm, engine, reorder.Flush())
	for _, ev := range wm.Flush() {
		engine.Append(ev)
	}
	qMu.Lock()
	finalQ := quarantine
	qMu.Unlock()

	m, err := finalize(reorder, builder, engine, packetQ, segQ, finalQ)
	if err != nil {
		return Metrics{}, err
	}
	// Pipeline latency p95 is dominated by the deliberately slow sink.
	m.PipelineLatencyP95Ms = stage2.LatencyP95().Milliseconds()
	return m, nil
}

func buildFixtureConfig(o Options) fixtures.Config {
	cfg := fixtures.Config{
		Seed:          o.Seed,
		DurationTicks: clock.FromDuration(o.Duration),
		FrameStep:     clock.Ticks(3600),
		SegmentStep:   clock.Ticks(2 * clock.Rate),
		LossRate:      o.InjectLoss,
	}
	switch o.Case {
	case fixtures.CaseTruncatedBox:
		cfg.TruncateBox = true
		cfg.LossRate = 0
	case fixtures.CaseTimestampWrap:
		cfg.Wrap = true
	case fixtures.CaseDuplicate:
		cfg.DuplicateRate = 0.2
	case fixtures.CaseLateOutOfOrder:
		cfg.LateRate = 1
	case fixtures.CaseLateOutsideWindow:
		cfg.LateOutside = true
		cfg.LateRate = 1
	case fixtures.CaseLoss:
		// loss already set via InjectLoss
	}
	return cfg
}

func handleOrdered(wm *timeline.Watermark, engine *timeline.ReplayEngine, ordered []ingest.OrderedPacket) {
	for _, op := range ordered {
		if op.IsGap {
			// Preserve the hole on the timeline as an untrusted gap
			// event; fieldcut never fabricates a frame for it.
			gap := &timeline.Event{
				TrackID:  0,
				Sequence: int(op.GapStart),
				Kind:     timeline.KindGap,
				Start:    0,
				Defect:   fmt.Sprintf("gap:%d", op.GapCount),
			}
			released, _ := wm.Commit(gap)
			for _, r := range released {
				engine.Append(r)
			}
			continue
		}
		ev := orderedEvent(op.Packet, 0, op.Recovered, op.Source)
		released, _ := wm.Commit(ev)
		for _, r := range released {
			engine.Append(r)
		}
	}
}

func orderedEvent(pkt *rtp.Packet, track int, recovered bool, src ingest.Source) *timeline.Event {
	return &timeline.Event{
		TrackID:   track,
		Sequence:  int(pkt.SequenceNumber),
		Kind:      timeline.KindFrame,
		RawTS:     pkt.Timestamp,
		Source:    src,
		Recovered: recovered,
	}
}

