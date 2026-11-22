// Package rtp reorders, de-duplicates and gap-analyses RTP packet traces.
//
// Sequence numbers and timestamps are 16/32-bit RTP fields that wrap; the
// reorderer expands them onto the monotonic 64-bit clock axis and bounds
// all buffering by a fixed window.
package rtp

import (
	"fmt"

	"github.com/pion/rtp"

	"mediascan/internal/clock"
	"mediascan/internal/fault"
)

// Packet is one observed RTP packet together with its arrival ordinal.
type Packet struct {
	Arrival int
	*rtp.Packet
}

// Burst is one consecutive run of missing extended sequence numbers.
type Burst struct {
	FromSeq   uint64
	ToSeq     uint64
	StartTick int64
	EndTick   int64
	Count     int
}

// Result is the reorder analysis outcome on the monotonic time axis.
type Result struct {
	Ordered    []Packet // packets in extended sequence order
	Duplicates int
	Reordered  int // arrival ordinal differs from sequence order
	Late       int // unseen packet arriving behind the contiguous head
	Bursts     []Burst
	MissingSeq []uint64
	Faults     []fault.Fault
	MinExtSeq  uint64
	MaxExtSeq  uint64
}

// Reorderer is a bounded-window RTP jitter buffer with gap accounting.
//
// Extended sequence numbers are anchored on maxExt (the highest extended
// seq observed), so legitimate burst jumps stay on the same 16-bit cycle
// while true wraps advance to the next. headSeq is the highest seq
// delivered contiguously; delivered tells whether any flush happened.
// No sentinel uint64 value is stored, so a first seq of 0 cannot underflow
// a scan loop.
type Reorderer struct {
	window    uint16
	clk       clock.Clock
	started   bool
	delivered bool
	firstExt  uint64
	headSeq   uint64
	maxExt    uint64
	buf       map[uint64]Packet
	seen      map[uint64]bool // extended seqs that arrived at least once
	booked    map[uint64]bool // missing seqs already recorded as loss
	lastTick  int64
	res       Result
	arrivals  int
}

// MaxWindow caps the reorder window to bound memory.
const MaxWindow uint16 = 8192

// NewReorderer validates and constructs a reorderer.
func NewReorderer(window uint16, clk clock.Clock) (*Reorderer, error) {
	if window == 0 || window > MaxWindow {
		return nil, fmt.Errorf("rtp: window %d outside (0,%d]", window, MaxWindow)
	}
	return &Reorderer{
		window: window, clk: clk,
		buf:    map[uint64]Packet{},
		seen:   map[uint64]bool{},
		booked: map[uint64]bool{},
		res:    Result{MinExtSeq: ^uint64(0)},
	}, nil
}

// Push feeds one received packet (already unmarshalled by pion/rtp).
func (r *Reorderer) Push(p *rtp.Packet) error {
	if p == nil {
		return fmt.Errorf("rtp: nil packet")
	}
	r.arrivals++
	pkt := Packet{Arrival: r.arrivals, Packet: p}
	if !r.started {
		r.firstExt = uint64(p.SequenceNumber)
		r.maxExt = r.firstExt
		r.started = true
	}
	ext := r.expandSeq(p.SequenceNumber)
	// A seq observed before is always a duplicate, buffered or flushed.
	if r.seen[ext] {
		r.res.Duplicates++
		where := "inside window"
		if r.delivered && ext <= r.headSeq {
			where = "outside window"
		}
		r.res.Faults = append(r.res.Faults, r.dupFault(ext, where))
		return nil
	}
	// An unseen seq behind the contiguous head can never join the ordered
	// stream: its slot already passed, so it is a late packet.
	if r.delivered && ext <= r.headSeq {
		r.res.Late++
		tick := clock.Expand32(r.lastTick, p.Timestamp)
		r.res.Faults = append(r.res.Faults, fault.Fault{
			Cat: fault.CatRTPLate, Ref: fmt.Sprintf("seq=%d", ext),
			Span: fault.Interval{Start: tick, End: tick + r.clk.TicksPerFrame()},
			Msg: fmt.Sprintf("packet %d arrived past window head %d", ext, r.headSeq),
		})
		return nil
	}
	r.seen[ext] = true
	// reordered arrival: a higher extended seq was already received while
	// this seq was still missing inside the accepted window
	if ext < r.maxExt {
		r.res.Reordered++
	}
	r.buf[ext] = pkt
	if ext > r.maxExt {
		r.maxExt = ext
	}
	r.advance()
	return nil
}

func (r *Reorderer) dupFault(ext uint64, where string) fault.Fault {
	return fault.Fault{
		Cat: fault.CatRTPDuplicate, Ref: fmt.Sprintf("seq=%d", ext),
		Msg: "duplicate extended sequence number " + where,
	}
}

// expandSeq lifts a 16-bit seq onto the 64-bit axis.
//
// The unsigned ring distance udelta = seq - prev16 (mod 2^16) decides the
// cycle. A forward distance under half the ring is normal progression on
// the current cycle; a distance over half the ring means seq actually
// sits one cycle behind (a duplicate, reorder or late packet). The only
// forward cycle advance is a genuine wrap, detected when seq's low bits
// fall below prev16 while the forward jump itself is small.
func (r *Reorderer) expandSeq(seq uint16) uint64 {
	if !r.started {
		return uint64(seq)
	}
	cycle := r.maxExt &^ 0xFFFF
	prev16 := uint16(r.maxExt)
	udelta := uint32(seq) - uint32(prev16)
	switch {
	case udelta == 0:
		return r.maxExt
	case udelta < 1<<15:
		// small forward step; if the raw value wrapped below prev16 the
		// trace crossed into the next 16-bit cycle (e.g. 0xFFFF -> 0x0000)
		if seq < prev16 {
			return cycle + 0x10000 + uint64(seq)
		}
		return cycle + uint64(seq)
	default:
		// large ring distance: packet belongs to an earlier position;
		// stay on the current cycle unless that would precede the stream
		same := cycle + uint64(seq)
		if same < r.firstExt {
			return same + 0x10000
		}
		return same
	}
}

// nextHead returns the seq the contiguous head expects next.
func (r *Reorderer) nextHead() uint64 {
	if r.delivered {
		return r.headSeq + 1
	}
	return r.firstExt
}

// advance flushes every contiguous head packet, then slides the window.
func (r *Reorderer) advance() {
	for {
		next := r.nextHead()
		pkt, ok := r.buf[next]
		if !ok {
			break
		}
		r.flush(pkt, next)
	}
	if !r.delivered {
		return // window cannot slide before the first contiguous flush
	}
	if r.maxExt-r.headSeq <= uint64(r.window) {
		return
	}
	newBase := r.maxExt - uint64(r.window)
	if newBase < r.firstExt {
		return
	}
	// bounded iteration count prevents any uint64 underflow loop
	for n := newBase - r.headSeq; n > 0; n-- {
		seq := r.headSeq + 1
		if !r.seen[seq] && !r.booked[seq] {
			r.recordMissing(seq)
		}
		delete(r.buf, seq)
		r.headSeq = seq
	}
}

func (r *Reorderer) flush(pkt Packet, seq uint64) {
	delete(r.buf, seq)
	r.headSeq = seq
	r.delivered = true
	r.lastTick = clock.Expand32(r.lastTick, pkt.Timestamp)
	if seq < r.res.MinExtSeq {
		r.res.MinExtSeq = seq
	}
	r.res.MaxExtSeq = seq
	r.res.Ordered = append(r.res.Ordered, pkt)
}

// Finalize closes the trace: every unbooked gap after the delivered head
// and up to maxExt is recorded as burst loss.
func (r *Reorderer) Finalize() *Result {
	if !r.started {
		return &r.res
	}
	lo := r.firstExt
	if r.delivered {
		lo = r.headSeq + 1
	}
	if lo > r.maxExt {
		r.buf = map[uint64]Packet{}
		return &r.res
	}
	for n := r.maxExt - lo + 1; n > 0; n-- {
		seq := lo + (r.maxExt - lo + 1 - n)
		if !r.seen[seq] && !r.booked[seq] {
			r.recordMissing(seq)
		}
	}
	r.buf = map[uint64]Packet{}
	return &r.res
}

func (r *Reorderer) recordMissing(seq uint64) {
	r.booked[seq] = true
	r.res.MissingSeq = append(r.res.MissingSeq, seq)
	if n := len(r.res.Bursts); n > 0 && r.res.Bursts[n-1].ToSeq+1 == seq {
		r.res.Bursts[n-1].ToSeq = seq
		r.res.Bursts[n-1].Count++
		r.res.Bursts[n-1].EndTick += r.clk.TicksPerFrame()
		return
	}
	tick := r.lastTick + r.clk.TicksPerFrame()
	r.res.Bursts = append(r.res.Bursts, Burst{
		FromSeq: seq, ToSeq: seq, Count: 1,
		StartTick: tick, EndTick: tick + r.clk.TicksPerFrame(),
	})
	r.res.Faults = append(r.res.Faults, fault.Fault{
		Cat: fault.CatBurstLoss, Ref: fmt.Sprintf("seq=%d", seq),
		Span: fault.Interval{Start: tick, End: tick + r.clk.TicksPerFrame()},
		Msg: fmt.Sprintf("burst loss starting at seq %d", seq),
	})
}
