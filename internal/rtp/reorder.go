package rtp

import "time"

const (
	// MaxReorderWindow is the hard upper bound on buffered packets.
	MaxReorderWindow = 512

	DefaultJitterWait = 60 * time.Millisecond
	MinJitterWait     = 15 * time.Millisecond
	MaxJitterWait     = 160 * time.Millisecond
	jitterStep        = 10 * time.Millisecond
)

// Packet is the decoded RTP packet carried between subsystems.
type Packet struct {
	Seq         uint16
	PayloadType uint8
	Marker      bool
	Timestamp   uint32
	SSRC        uint32
	Payload     []byte
	Arrival     time.Duration
}

// Gap describes a contiguous run of sequence numbers that never arrived.
type Gap struct {
	Start uint16
	Count int
}

// Reorderer turns an out-of-order RTP flow into in-order packets. It handles
// 16-bit sequence wraparound, duplicates and reordering, and waits at most a
// bandwidth-controlled jitter interval before declaring a gap.
type Reorderer struct {
	maxWindow int
	wait      time.Duration
	bandwidth int64 // bits per second

	init    bool
	base    int64
	highest int64
	next    int64
	now     time.Duration
	buf     map[int64]Packet
	gapAt   time.Duration
	haveGap bool

	// Repair is consulted before a one-packet gap is declared unrecoverable.
	// When it returns packets covering the gap they are inserted into the
	// stream and the flow continues without an unrecoverable interval.
	Repair func(Gap) ([]Packet, bool)

	released   bool
	Duplicates int
	Late       int
	Declared   []Gap
	Repaired   []Gap
}

// NewReorderer builds a reorderer with the default jitter target.
func NewReorderer() *Reorderer { return NewReordererWithWindow(MaxReorderWindow) }

// NewReordererWithWindow builds a reorderer with a bounded window.
func NewReordererWithWindow(n int) *Reorderer {
	if n <= 0 || n > MaxReorderWindow {
		n = MaxReorderWindow
	}
	return &Reorderer{maxWindow: n, wait: DefaultJitterWait, buf: make(map[int64]Packet)}
}

// SetBandwidth feeds the observed bitrate into the jitter controller. Low
// bandwidth widens the wait, high bandwidth narrows it, and every adjustment
// is clamped to the configured bounds.
func (r *Reorderer) SetBandwidth(bps int64) {
	r.bandwidth = bps
	var target time.Duration
	switch {
	case bps <= 0:
		target = DefaultJitterWait
	case bps < 2_000_000:
		target = 120 * time.Millisecond
	case bps < 8_000_000:
		target = 60 * time.Millisecond
	default:
		target = 30 * time.Millisecond
	}
	switch {
	case target > r.wait+jitterStep:
		r.wait += jitterStep
	case target < r.wait/jitterStep*jitterStep:
		r.wait -= jitterStep
	}
	if r.wait < MinJitterWait {
		r.wait = MinJitterWait
	}
	if r.wait > MaxJitterWait {
		r.wait = MaxJitterWait
	}
}

// Wait returns the current jitter wait.
func (r *Reorderer) Wait() time.Duration { return r.wait }

// Window returns the configured packet window.
func (r *Reorderer) Window() int { return r.maxWindow }

func (r *Reorderer) extend(seq uint16) int64 {
	if !r.init {
		r.init = true
		r.base = int64(seq)
		r.highest = r.base
		r.next = r.base
		return r.base
	}
	ext := r.highest + int64(int16(seq-uint16(r.highest)))
	if ext > r.highest {
		r.highest = ext
	}
	return ext
}

func (r *Reorderer) nextBuffered() (int64, bool) {
	var min int64
	found := false
	for ext := range r.buf {
		if !found || ext < min {
			min, found = ext, true
		}
	}
	return min, found
}

func (r *Reorderer) tryRepair(g Gap) bool {
	if r.Repair == nil || g.Count != 1 {
		return false
	}
	pkts, ok := r.Repair(g)
	if !ok || len(pkts) == 0 {
		return false
	}
	for _, p := range pkts {
		ext := r.next + int64(int16(p.Seq-uint16(r.next&0xffff)))
		if ext < r.next || ext >= r.next+int64(g.Count) {
			return false
		}
		p.Arrival = r.now - 2*r.wait
		if _, exists := r.buf[ext]; !exists {
			r.buf[ext] = p
		}
	}
	r.Repaired = append(r.Repaired, g)
	return true
}

// drain releases contiguous packets that have aged past the jitter wait.
// Packets are held rather than emitted eagerly so a reordered packet that
// arrives just after its successor is still accepted.
func (r *Reorderer) drain(force bool) []Packet {
	var out []Packet
	for {
		if p, ok := r.buf[r.next]; ok {
			if !force && p.Arrival > r.now-r.wait {
				return out
			}
			delete(r.buf, r.next)
			out = append(out, p)
			r.released = true
			r.next++
			r.haveGap = false
			r.gapAt = 0
			continue
		}
		if len(r.buf) == 0 {
			return out
		}
		min, ok := r.nextBuffered()
		if !ok || min <= r.next {
			return out
		}
		g := Gap{Start: uint16(r.next & 0xffff), Count: int(min - r.next)}
		if g.Count > r.maxWindow {
			g.Count = r.maxWindow
		}
		if r.tryRepair(g) {
			r.haveGap = false
			continue
		}
		if !r.haveGap {
			r.haveGap = true
			r.gapAt = r.now
		}
		if !force && r.now-r.gapAt < r.wait && len(r.buf) < r.maxWindow {
			return out
		}
		r.Declared = append(r.Declared, g)
		r.next = min
		r.haveGap = false
	}
}

// Push accepts one packet and returns packets that are safe to emit plus any
// gaps that expired. The arrival timestamp drives the jitter deadline.
func (r *Reorderer) Push(p Packet) ([]Packet, []Gap) {
	ext := r.extend(p.Seq)
	// A missing arrival timestamp gets a synthetic 1 ms cadence so callers
	// that only care about ordering still get bounded latency.
	if p.Arrival <= r.now {
		r.now += time.Millisecond
		p.Arrival = r.now
	} else {
		r.now = p.Arrival
	}
	if ext < r.next {
		// Before the first release the window may rewind to absorb packets
		// that were reordered ahead of the stream start.
		if !r.released && ext >= r.base-int64(r.maxWindow) {
			r.next = ext
			r.buf[ext] = p
			before := len(r.Declared)
			out := r.drain(false)
			return out, append([]Gap(nil), r.Declared[before:]...)
		}
		r.Late++
		return nil, nil
	}
	if old, ok := r.buf[ext]; ok {
		r.Duplicates++
		before := len(r.Declared)
		if len(p.Payload) > len(old.Payload) {
			r.buf[ext] = p
		}
		out := r.drain(false)
		return out, append([]Gap(nil), r.Declared[before:]...)
	}
	r.buf[ext] = p
	force := len(r.buf) >= r.maxWindow
	before := len(r.Declared)
	out := r.drain(force)
	gaps := append([]Gap(nil), r.Declared[before:]...)
	if len(gaps) == 0 {
		return out, nil
	}
	return out, gaps
}
