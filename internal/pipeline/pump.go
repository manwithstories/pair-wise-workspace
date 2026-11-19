package pipeline

import (
	"avpipe/internal/audio"
	"avpipe/internal/clock"
	"avpipe/internal/fault"
	"avpipe/internal/stream"
)

// Pump drives a real bounded queue with a virtual consumer. Media arrival
// times come from the block's start tick plus jitter; the consumer retires
// one block every ServiceMs of virtual time. Because producer and consumer
// advance on the same virtual clock, the queue depth reflects genuine
// backlog rather than "everything arrived at once", and the byte/duration
// limits and the policy are exercised exactly as in a live stream.
type Pump struct {
	Queue      *stream.Queue
	ServiceMs  float64 // time the consumer needs per block (block cadence)
	ConsumerMs float64 // configured playout latency added to every block
}

// PumpResult is the outcome of one pumped stream.
type PumpResult struct {
	Blocks    []audio.Block
	Latencies []float64 // per-block residence time in milliseconds
	Dropped   int64
	Evicted   int64
	PeakBytes int64
	PeakTicks int64
}

type pumpEntry struct {
	block   audio.Block
	arrival float64
}

// Run pushes every block at its arrival time and retires blocks as the
// consumer becomes free. fullPolicy is the queue's own policy string.
func (p *Pump) Run(blocks []audio.Block, arrivals []float64) (PumpResult, error) {
	if len(blocks) != len(arrivals) {
		return PumpResult{}, fault.New(fault.CodeInternal, "pipeline.pump",
			"%d blocks but %d arrival times", len(blocks), len(arrivals))
	}
	var res PumpResult
	freeAt := 0.0
	service := func(e pumpEntry) {
		start := freeAt
		if start < e.arrival {
			start = e.arrival
		}
		freeAt = start + p.ServiceMs
		res.Latencies = append(res.Latencies, freeAt-e.arrival+p.ConsumerMs)
		res.Blocks = append(res.Blocks, e.block)
	}
	// retireAll drains the queue head into the consumer until it would need
	// to start after now.
	retireAll := func(now float64) error {
		for p.Queue.Len() > 0 && freeAt <= now {
			it, ok := p.Queue.Pop()
			if !ok {
				return fault.New(fault.CodeInternal, "pipeline.pump", "queue closed mid-run")
			}
			service(it.Value.(pumpEntry))
		}
		return nil
	}

	for i, b := range blocks {
		a := arrivals[i]
		if err := retireAll(a); err != nil {
			return res, err
		}
		item := stream.Item{Bytes: b.Bytes(), Span: clock.Span{Start: b.StartTick, End: b.EndTick()},
			Label: "audio", Value: pumpEntry{block: b, arrival: a}}
		ok, err := p.Queue.TryPush(item)
		if err != nil {
			return res, err
		}
		for !ok {
			switch p.Queue.Policy() {
			case stream.Drop:
				res.Dropped++
				ok = true // stop trying
			case stream.DropOldest:
				// Evicted blocks are discarded, not delivered, so they do
				// not pass through the consumer.
				if _, got := p.Queue.Pop(); !got {
					return res, fault.New(fault.CodeInternal, "pipeline.pump", "queue closed while evicting")
				}
				res.Evicted++
				ok, err = p.Queue.TryPush(item)
				if err != nil {
					return res, err
				}
				if !ok && p.Queue.Len() == 0 {
					// The block alone exceeds capacity.
					res.Dropped++
					ok = true
				}
			default: // block: wait for the consumer to make room
				if p.Queue.Len() == 0 {
					return res, fault.New(fault.CodeQueueLimit, "pipeline.pump",
						"block of %d bytes / %d ticks exceeds an empty queue",
						b.Bytes(), int64(b.EndTick()-b.StartTick))
				}
				it, got := p.Queue.Pop()
				if !got {
					return res, fault.New(fault.CodeInternal, "pipeline.pump", "queue closed while waiting")
				}
				service(it.Value.(pumpEntry))
				ok, err = p.Queue.TryPush(item)
				if err != nil {
					return res, err
				}
			}
		}
	}
	// Producer finished: drain the remainder through the consumer.
	for p.Queue.Len() > 0 {
		it, ok := p.Queue.Pop()
		if !ok {
			break
		}
		service(it.Value.(pumpEntry))
	}
	st := p.Queue.Stats()
	res.PeakBytes, res.PeakTicks = st.PeakBytes, st.PeakTicks
	return res, nil
}
