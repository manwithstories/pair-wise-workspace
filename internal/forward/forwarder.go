// Package forward fans one ingress RTP stream out to multiple egress
// tracks with per-egress SSRC/sequence/timestamp rewriting.
package forward

import (
	"sync"
	"sync/atomic"

	rtp "github.com/pion/rtp/v2"
)

// Egress is one output leg with its own bounded queue and rewrite state.
type Egress struct {
	id       int
	ssrc     uint32
	seq      uint16
	tsOffset uint32
	queue    chan []byte
	write    func([]byte) error
	dead     atomic.Bool
}

// Forwarder copies ingress packets to every egress without blocking the
// reader: a full queue drops the oldest queued packet.
type Forwarder struct {
	mu        sync.Mutex
	egresses  []*Egress
	queueSize int

	forwarded  atomic.Uint64
	dropped    atomic.Uint64
	egressErrs atomic.Uint64

	wg     sync.WaitGroup
	closed atomic.Bool
}

// New creates a Forwarder whose egress queues hold up to queueSize packets.
func New(queueSize int) *Forwarder {
	if queueSize < 1 {
		queueSize = 1
	}
	return &Forwarder{queueSize: queueSize}
}

// AddEgress registers a new output leg; write delivers one RTP packet.
// It returns the egress id.
func (f *Forwarder) AddEgress(ssrcBase uint32, write func([]byte) error) int {
	f.mu.Lock()
	id := len(f.egresses)
	e := &Egress{
		id:       id,
		ssrc:     ssrcBase + uint32(id),
		tsOffset: uint32(id) * 7919,
		queue:    make(chan []byte, f.queueSize),
		write:    write,
	}
	f.egresses = append(f.egresses, e)
	f.mu.Unlock()
	f.wg.Add(1)
	go f.pump(e)
	return id
}

// Forward rewrites and enqueues one ingress RTP packet for every egress.
func (f *Forwarder) Forward(data []byte) {
	var pkt rtp.Packet
	if err := pkt.Unmarshal(data); err != nil {
		return
	}
	f.forwarded.Add(1)
	f.mu.Lock()
	egresses := make([]*Egress, len(f.egresses))
	copy(egresses, f.egresses)
	f.mu.Unlock()
	for _, e := range egresses {
		out := pkt
		out.Header.SSRC = e.ssrc
		out.Header.SequenceNumber = e.nextSeq()
		out.Header.Timestamp += e.tsOffset
		buf, err := out.Marshal()
		if err != nil {
			continue
		}
		select {
		case e.queue <- buf:
		default:
			select {
			case <-e.queue: // drop oldest, never block the reader
			default:
			}
			e.queue <- buf
			f.dropped.Add(1)
		}
	}
}

func (e *Egress) nextSeq() uint16 {
	s := e.seq
	e.seq++
	return s
}

func (f *Forwarder) pump(e *Egress) {
	defer f.wg.Done()
	for buf := range e.queue {
		if e.dead.Load() {
			continue
		}
		if err := e.write(buf); err != nil {
			// A single failing leg only affects itself.
			e.dead.Store(true)
			f.egressErrs.Add(1)
		}
	}
}

// Close shuts the egress queues and waits for writers to drain.
func (f *Forwarder) Close() {
	if f.closed.Swap(true) {
		return
	}
	f.mu.Lock()
	for _, e := range f.egresses {
		close(e.queue)
	}
	f.mu.Unlock()
	f.wg.Wait()
}

// Stats returns (forwarded, dropped, egressErrors) counters.
func (f *Forwarder) Stats() (uint64, uint64, uint64) {
	return f.forwarded.Load(), f.dropped.Load(), f.egressErrs.Load()
}
