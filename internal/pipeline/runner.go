package pipeline

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"mediacheck/internal/ingest"
	"mediacheck/internal/inspect"
	"mediacheck/internal/model"
	"mediacheck/internal/recover"
)

// Run drives the fixture through ingest -> inspect -> recover.
//
// The reader's EmitFunc pushes into the bounded queue; when the consumer is
// slow the queue fills and Push blocks, which blocks Emit and therefore the
// read stage.  Cancellation is delivered through the context, and every
// goroutine is joined before Run returns so FFmpeg/libopus handles can be
// released without racing a stage that is still using them.
func Run(ctx context.Context, g *ingest.Generated, opts Options) (*Result, error) {
	if g == nil {
		return nil, fmt.Errorf("pipeline: nil fixture")
	}
	if opts.QueueItems <= 0 {
		opts.QueueItems = 16
	}
	start := time.Now()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	reader, err := ingest.NewReader(g.Fixture(), ingest.DefaultReaderLimits())
	if err != nil {
		return nil, err
	}
	queue := NewQueue(opts.QueueItems, opts.QueueBytes)
	queue.Bind(ctx)

	res := &Result{
		TrackSpan: map[model.Track]int64{},
	}
	var mu sync.Mutex
	insp := inspect.New()
	lat := newLatencyWindow(opts.LatencyWindow)

	var filler *recover.AudioFiller
	tracker := recover.NewTracker()
	// Recovery is Opus-specific: FEC and PLC are libopus features and the
	// filler re-encodes placeholders as Opus.  Running it over AAC would
	// fabricate garbage, so AAC fixtures report a clean prefix only and the
	// capability block states what was actually active.
	codec := g.Config.Codec
	if opts.Recovery && codec == model.CodecOpus {
		filler, res.Capabilities, err = recover.NewAudioFiller()
		if err != nil {
			// Degrade rather than fail the run: the report states which
			// capabilities were unavailable.
			filler = nil
			res.Capabilities = recover.Capabilities{Silence: false}
		} else {
			defer filler.Close()
		}
	}

	var wg sync.WaitGroup
	var readErr, consumeErr error
	var firstSlot, lastSlot int64
	var readLostSlots []model.Slot
	haveSlot := false

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer queue.Close()
		readErr = reader.Run(ctx, func(ev ingest.Event) error {
			if ev.Fault != nil {
				mu.Lock()
				res.Faults = append(res.Faults, *ev.Fault)
				if ev.Fault.Kind == model.FaultLoss {
					readLostSlots = append(readLostSlots, ev.Fault.Slot)
				}
				mu.Unlock()
				return nil
			}
			if ev.Packet == nil {
				return nil
			}
			return queue.Push(*ev.Packet)
		})
	}()

	trackEnd := map[model.Track]int64{}
	var earlyDamage model.DamageSet
	consumeErr = consume(ctx, queue, opts, insp, filler, tracker, lat, &firstSlot, &lastSlot, &haveSlot, trackEnd, &earlyDamage, &mu)
	// The consumer may stop early (a mux write error, a cancelled context)
	// while the reader is still blocked in Push. Closing the queue and
	// cancelling the run context releases it; otherwise wg.Wait would hang
	// on a producer nobody is draining.
	queue.Close()
	canceledByCaller := ctx.Err() != nil
	cancel()
	wg.Wait()

	// The consumer's own error is the root cause when it stopped early; the
	// reader's ErrQueueClosed is merely the consequence of us closing the
	// queue to release it. Report the root cause first so a disk write
	// failure is never disguised as a queue shutdown.
	if consumeErr != nil && !errors.Is(consumeErr, model.ErrCanceled) {
		return nil, consumeErr
	}
	if readErr != nil && !errors.Is(readErr, model.ErrCanceled) && !errors.Is(readErr, ErrQueueClosed) {
		return nil, readErr
	}
	res.Canceled = canceledByCaller || errors.Is(readErr, model.ErrCanceled) || errors.Is(consumeErr, model.ErrCanceled)
	res.Queue = queue.Stats()
	res.Latency = lat.stats()
	res.Elapsed = time.Since(start)
	res.Inspection = insp.Finish()
	res.Recovery = tracker.Outcomes()
	if filler == nil {
		// Without an Opus filler every audio gap stays missing. Report those
		// as unfilled slots rather than a vacuous 100% recovery.
		for _, lf := range readLostSlots {
			res.Recovery.Slots++
			res.Recovery.Failed++
			res.Damage.AddRange(lf.Start, lf.End, model.FaultLoss)
		}
	}
	res.Packets = res.Inspection.Streams[model.TrackAudio].Packets + res.Inspection.Streams[model.TrackVideo].Packets
	res.Bytes = res.Inspection.Streams[model.TrackAudio].Bytes + res.Inspection.Streams[model.TrackVideo].Bytes
	res.FirstSlot, res.LastSlot = firstSlot, lastSlot
	for track, end := range trackEnd {
		res.TrackSpan[track] = end - res.FirstSlot
	}
	res.Faults = append(res.Faults, res.Recovery.Faults()...)
	for _, r := range res.Recovery.Damage() {
		res.Damage.AddRange(r.Start, r.End, r.Kind)
	}
	for _, f := range res.Inspection.Faults {
		res.Damage.Add(f)
	}
	for _, r := range earlyDamage.Ranges() {
		res.Damage.AddRange(r.Start, r.End, r.Kind)
	}
	return res, nil
}

func consume(ctx context.Context, q *Queue, opts Options, insp *inspect.Inspector, filler *recover.AudioFiller,
	tracker *recover.Tracker, lat *latencyWindow, firstSlot, lastSlot *int64, haveSlot *bool,
	trackEnd map[model.Track]int64, damage *model.DamageSet, mu *sync.Mutex) error {
	// lastAudio remembers the last real audio packet so a hole can be filled
	// with the right frame size and FEC look-ahead.
	var lastAudio *model.Packet
	for {
		p, ok, err := q.Pop()
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		began := time.Now()
		if opts.SlowConsumer > 0 {
			select {
			case <-time.After(opts.SlowConsumer):
			case <-ctx.Done():
				return fmt.Errorf("%w: %v", model.ErrCanceled, ctx.Err())
			}
		}
		mu.Lock()
		if !*haveSlot {
			*firstSlot = p.Slot.Start
			*haveSlot = true
		}
		if p.Slot.Start > *lastSlot {
			*lastSlot = p.Slot.Start
		}
		if p.Slot.End > trackEnd[p.Track] {
			trackEnd[p.Track] = p.Slot.End
		}
		mu.Unlock()

		// Inspect first: it flags payloads whose codec structure is broken,
		// and those must not be muxed as-is.  The packet is still counted
		// and its slot reported damaged, which is what keeps later packets
		// reachable.
		p = insp.Inspect(p)
		dropVideo := p.Corrupt && p.Track == model.TrackVideo
		if dropVideo {
			// A video access unit with an unparsable NAL structure cannot
			// be repaired. Record the hole and leave it out of the
			// deliverable instead of muxing a frame every decoder rejects.
			damage.AddRange(p.Slot.Start, p.Slot.End, model.FaultBadLength)
		}

		if p.Track == model.TrackAudio {
			if filler != nil && !p.Corrupt && lastAudio != nil && p.Slot.Start > lastAudio.Slot.End {
				gap := p.Slot.Start - lastAudio.Slot.End
				for _, slot := range splitGap(lastAudio.Slot.End, gap, lastAudio.Slot.Duration()) {
					out := filler.Fill(slot, int(lastAudio.Duration), p.Data)
					tracker.Record(out)
					if out.Method != recover.MethodGiveUp && opts.Emit != nil {
						// The placeholder is a real Opus packet covering the
						// missing slot, so the output timeline has neither a
						// hole nor overlapping timestamps.
						if err := opts.Emit(recoveredPacket(p, slot, out.Data)); err != nil {
							return err
						}
					}
				}
			}
			if filler != nil && !p.Corrupt {
				if err := filler.Observe(p.Data, int(p.Duration)); err != nil {
					// A damaged packet cannot prime the decoder; the next
					// real packet re-primes it. Record the failure honestly
					// rather than pretending the slot was recovered.
					tracker.Record(recover.SlotResult{Slot: p.Slot, Method: recover.MethodGiveUp,
						Reason: "decoder rejected packet"})
				}
			}
			cp := p
			lastAudio = &cp
		}

		if opts.Emit != nil && !dropVideo {
			if err := opts.Emit(p); err != nil {
				return err
			}
		}
		lat.add(time.Since(began))
	}
}
