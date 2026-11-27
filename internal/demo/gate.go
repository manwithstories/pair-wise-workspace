package demo

import (
	"sync"
	"time"

	"avpipe/internal/media"
)

// startGate models a consumer that is not yet keeping up. It refuses to take
// its first block until the producer has actually blocked on the full bounded
// queue. Waiting on the queue's own blocked signal (rather than sleeping for a
// guessed duration) makes backpressure observable deterministically, even
// under the race detector on a fast host.
type startGate struct {
	once     sync.Once
	full     <-chan struct{}
	blocked  <-chan struct{}
	timeout  time.Duration
	waited   bool
	fullSeen bool
}

func newStartGate(timeout time.Duration) *startGate {
	return &startGate{timeout: timeout}
}

// Bind installs the queue's signals. The runner calls it before producing.
func (g *startGate) Bind(full, blocked <-chan struct{}) {
	g.full = full
	g.blocked = blocked
}

// Wait blocks once, before the first block is consumed, until either the
// producer has blocked on a full queue or the safety timeout expires.
func (g *startGate) Wait() {
	if g == nil {
		return
	}
	g.once.Do(func() {
		timer := time.NewTimer(g.timeout)
		defer timer.Stop()
		if g.blocked == nil {
			<-timer.C
			return
		}
		select {
		case <-g.blocked:
			g.waited = true
		case <-timer.C:
			return
		}
		if g.full != nil {
			select {
			case <-g.full:
				g.fullSeen = true
			default:
			}
		}
	})
}

// Sat reports whether the queue was observed full and a producer blocked.
func (g *startGate) Sat() bool { return g.waited && g.fullSeen }

// gatedSink holds its first block at the gate, then stalls by the configured
// per-block delay. The inner sink does the real rendering and auditing.
type gatedSink struct {
	inner Sink
	gate  *startGate
	delay time.Duration
	sleep func(time.Duration)
}

// Sink is the consumer contract of the pipeline.
type Sink interface {
	Consume(media.Block) error
}

// Consume waits for the gate, applies the stall, then forwards the block.
func (s *gatedSink) Consume(b media.Block) error {
	s.gate.Wait()
	if s.delay > 0 {
		if s.sleep != nil {
			s.sleep(s.delay)
		} else {
			time.Sleep(s.delay)
		}
	}
	return s.inner.Consume(b)
}
