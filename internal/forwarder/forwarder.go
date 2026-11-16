// Package forwarder moves RTP packets from a producer to a consumer through a
// bounded queue.
//
// The queue is the point of the exercise: under load it must drop rather than
// grow, the drop must be counted as backpressure rather than folded into
// network loss, and Close must release the reader goroutine and the socket
// without a send-on-closed-channel race.
package forwarder

import (
	"errors"
	"sync"
	"sync/atomic"
)

// ErrClosed is returned by Push after Close and by Write once the queue is
// draining.
var ErrClosed = errors.New("forwarder is closed")

// Packet is the unit of forwarding. Keeping it an interface-free struct lets
// the caller decide how to marshal (RTP, raw bytes) without this package
// importing Pion.
type Packet struct {
	SSRC      uint32
	Sequence  uint16
	Timestamp uint32
	Payload   []byte
	// Raw is the serialized packet, forwarded unchanged.
	Raw []byte
	// Duplicate marks a deliberate re-send of a packet already forwarded. It
	// is counted separately from Forwarded, so "forwarded" means distinct
	// packets delivered while "retransmits" is the duplicate overhead.
	Duplicate bool
}

// Stats is the forwarder's accounting snapshot.
type Stats struct {
	Enqueued     uint64 `json:"enqueued"`
	Forwarded    uint64 `json:"forwarded"`
	Retransmits  uint64 `json:"retransmits"`
	Backpressure uint64 `json:"backpressure_dropped"`
	Errors       uint64 `json:"forward_errors"`
	QueueDepth   int    `json:"queue_depth"`
	QueueCap     int    `json:"queue_capacity"`
	Closed       bool   `json:"closed"`
}

// Forwarder is a bounded SPSC-ish queue: many producers may Push, a single
// consumer goroutine calls Process via Run.
type Forwarder struct {
	queue chan Packet
	sink  func(Packet) error
	done  chan struct{}
	once  sync.Once

	enqueued     atomic.Uint64
	forwarded    atomic.Uint64
	retransmits  atomic.Uint64
	backpressure atomic.Uint64
	errs         atomic.Uint64
	wg           sync.WaitGroup
}

// New creates a forwarder whose consumer is sink. capacity bounds the queue;
// a non-positive capacity is clamped to 1 so the queue always exists.
func New(capacity int, sink func(Packet) error) *Forwarder {
	if capacity <= 0 {
		capacity = 1
	}
	return &Forwarder{
		queue: make(chan Packet, capacity),
		sink:  sink,
		done:  make(chan struct{}),
	}
}

// Start launches the consumer goroutine. It returns immediately.
func (f *Forwarder) Start() {
	f.wg.Add(1)
	go f.consume()
}

func (f *Forwarder) consume() {
	defer f.wg.Done()
	for {
		// Serve the queue first. Checking done only when the queue is empty
		// is what makes Close drain rather than abandon: the two cases in one
		// select would let Go pick the done arm forever once it is closed,
		// stranding every queued packet.
		select {
		case p := <-f.queue:
			f.deliver(p)
			continue
		default:
		}
		select {
		case p := <-f.queue:
			f.deliver(p)
		case <-f.done:
			f.drain()
			return
		}
	}
}

// drain delivers whatever is already queued. Producers are already stopped by
// the time it runs (done is closed and Push refuses), so a non-blocking sweep
// is enough.
func (f *Forwarder) drain() {
	for {
		select {
		case p := <-f.queue:
			f.deliver(p)
		default:
			return
		}
	}
}

func (f *Forwarder) deliver(p Packet) {
	if f.sink != nil {
		if err := f.sink(p); err != nil {
			f.errs.Add(1)
			return
		}
	}
	if p.Duplicate {
		f.retransmits.Add(1)
		return
	}
	f.forwarded.Add(1)
}

// Push offers a packet to the queue. It never blocks: when the queue is full
// the packet is dropped and counted as backpressure, which is what keeps the
// queue bounded under a slow consumer.
func (f *Forwarder) Push(p Packet) error {
	select {
	case <-f.done:
		return ErrClosed
	default:
	}
	select {
	case f.queue <- p:
		f.enqueued.Add(1)
		return nil
	case <-f.done:
		return ErrClosed
	default:
		f.backpressure.Add(1)
		return nil
	}
}

// Close stops accepting, drains the queue and waits for the consumer to
// finish. It is idempotent and safe to call concurrently with Push: Push
// selects on done as well, so there is no send-on-closed-channel window.
func (f *Forwarder) Close() error {
	f.once.Do(func() {
		close(f.done)
		// Do not close(f.queue): producers may still be inside a select on it,
		// and closing while they hold a send arm would panic. The consumer
		// exits via done instead, and the channel is reclaimed by GC.
	})
	f.wg.Wait()
	return nil
}

// Stats returns the current counters. QueueDepth is a point-in-time length.
func (f *Forwarder) Stats() Stats {
	closed := false
	select {
	case <-f.done:
		closed = true
	default:
	}
	return Stats{
		Enqueued:     f.enqueued.Load(),
		Forwarded:    f.forwarded.Load(),
		Retransmits:  f.retransmits.Load(),
		Backpressure: f.backpressure.Load(),
		Errors:       f.errs.Load(),
		QueueDepth:   len(f.queue),
		QueueCap:     cap(f.queue),
		Closed:       closed,
	}
}
