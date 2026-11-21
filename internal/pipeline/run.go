// Package pipeline wires every stage into one deterministic offline run:
// ingest -> jitter/recovery -> backpressure queue -> timeline mix -> render
// -> MP4 export, then aggregates the acceptance metrics.
package pipeline

import (
	"context"
	"io"

	"avcheck/internal/clock"
	"avcheck/internal/fixture"
	"avcheck/internal/flow"
	"avcheck/internal/ingest"
	"avcheck/internal/jitter"
	"avcheck/internal/metrics"
	"avcheck/internal/recovery"
)

// Params configures one full pipeline run.
type Params struct {
	Fixture    fixture.Config
	QueueBytes int
	WindowPkts int
	Out        io.Writer
}

// Result carries the run's report and stage diagnostics.
type Result struct {
	Report         metrics.Report
	Queue          flow.Snapshot
	LatePackets    int64
	AcceptedBlocks int64
	Recovered      recovery.Stats
	SyncErrorTicks int64
}

// stage holds one track's ingest/recovery machinery.
type stage struct {
	reasm  *recovery.Reassembler
	parser *ingest.Parser
}

// Run executes a complete offline pass over the generated scenario. The
// backpressure path is driven deterministically in one goroutine: a full queue
// is drained (the accepted blocks are retained) before the enqueue is retried,
// so identical inputs always produce identical queue peaks and summaries.
func Run(ctx context.Context, p Params, packets []fixture.Packet) (*Result, error) {
	if p.WindowPkts < 1 {
		p.WindowPkts = 8
	}
	cfg := p.Fixture
	queue := flow.NewQueue(p.QueueBytes)
	stages := make(map[uint32]*stage)
	lat := &metrics.Latency{}
	var latePackets int64
	allBlocks := make([]*clock.Block, 0)

	// enqueue applies deterministic drain-and-retry backpressure.
	enqueue := func(b *clock.Block, arrivalMS int64) error {
		ok, err := queue.TryEnqueue(b, arrivalMS)
		if err != nil {
			return err
		}
		if !ok {
			queue.RecordBackpressure(1)
			for popped := queue.Pop(); popped != nil; popped = queue.Pop() {
				allBlocks = append(allBlocks, popped)
			}
			ok, err = queue.TryEnqueue(b, arrivalMS)
			if err != nil {
				return err
			}
			if !ok {
				return clock.NewError(clock.KindQueueFull,
					"block still does not fit after draining")
			}
		}
		return nil
	}

	for _, pkt := range packets {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		st, ok := stages[pkt.TrackID]
		if !ok {
			parser := ingest.NewParser(cfg.Channels)
			parser.BindTrack(pkt.SSRC, pkt.TrackID)
			st = &stage{
				parser: parser,
				reasm: recovery.NewReassembler(pkt.SSRC, pkt.TrackID,
					cfg.Channels, cfg.FramesPerPkt, p.WindowPkts),
			}
			stages[pkt.TrackID] = st
		}
		blk, err := st.parser.Parse(pkt.Raw, pkt.ArrivalMS)
		if err != nil {
			return nil, err
		}
		outcome, _ := st.reasm.Offer(blk)
		switch outcome {
		case jitter.OutcomeDuplicate:
			continue // collapsed, never enters the queue
		case jitter.OutcomeLate, jitter.OutcomeOverflow:
			latePackets++
			continue
		}
		for _, eb := range st.reasm.Emit() {
			latency := float64(pkt.ArrivalMS) -
				float64(eb.StartTick)*1000/float64(clock.SampleRate)
			if latency < 0 {
				latency = 0 // virtual clocks cannot report negative latency
			}
			lat.Add(latency)
			if err := enqueue(eb, pkt.ArrivalMS); err != nil {
				return nil, err
			}
		}
	}

	// End of stream: flush trailing gaps per track in deterministic order.
	total := cfg.SampleCount()
	endSeq := uint16(total / cfg.FramesPerPkt)
	for id := uint32(1); int(id) <= cfg.Tracks; id++ {
		st, ok := stages[id]
		if !ok {
			continue
		}
		for _, eb := range st.reasm.Flush(endSeq) {
			if err := enqueue(eb, 0); err != nil {
				return nil, err
			}
		}
	}
	for popped := queue.Pop(); popped != nil; popped = queue.Pop() {
		allBlocks = append(allBlocks, popped)
	}
	queue.Close()

	return finalize(p, stages, allBlocks, queue, lat, latePackets)
}
