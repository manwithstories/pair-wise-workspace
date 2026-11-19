// Package rtprepair reorders, de-duplicates and gap-tracks RTP packet streams
// while honouring a bounded reorder window so a stalled packet can never block
// emission indefinitely.
package rtprepair

import (
	"bytes"
	"fmt"
	"math"

	"github.com/pion/rtp"

	"mediasync/internal/media"
)

// Limits bound buffering and parsing work.
const (
	DefaultWindow      = 64
	DefaultMaxBuffered = 4096
	DefaultMaxPacket   = 2048
	MaxWindow          = 1 << 15
)

// Options configures a Repairer.
type Options struct {
	Window         int
	MaxBuffered    int
	MaxPacketBytes int
}

// DefaultOptions returns conservative bounds.
func DefaultOptions() Options {
	return Options{Window: DefaultWindow, MaxBuffered: DefaultMaxBuffered, MaxPacketBytes: DefaultMaxPacket}
}

func (o Options) normalize() Options {
	if o.Window <= 0 {
		o.Window = DefaultWindow
	}
	if o.Window > MaxWindow {
		o.Window = MaxWindow
	}
	if o.MaxBuffered <= 0 {
		o.MaxBuffered = DefaultMaxBuffered
	}
	if o.MaxPacketBytes <= 0 {
		o.MaxPacketBytes = DefaultMaxPacket
	}
	return o
}

// Packet is one received RTP packet with its arrival metadata.
type Packet struct {
	Raw       []byte     `json:"-"`
	Header    rtp.Header `json:"header"`
	Payload   []byte     `json:"-"`
	Seq       uint32     `json:"extended_seq"`
	SSRC      uint32     `json:"ssrc"`
	Offset    int64      `json:"offset"`
	ArrivalNS int64      `json:"arrival_ns"`
}

// Stats summarises what the repairer observed.
type Stats struct {
	Received         int   `json:"received"`
	Emitted          int   `json:"emitted"`
	DuplicatePackets int   `json:"duplicate_packets"`
	ReorderedPackets int   `json:"reordered_packets"`
	LostPackets      int   `json:"lost_packets"`
	BurstLostPackets int   `json:"burst_lost_packets"`
	LatePackets      int   `json:"late_packets"`
	MalformedPackets int   `json:"malformed_packets"`
	MaxBufferDepth   int   `json:"max_buffer_depth"`
	RoundtripOK      int   `json:"roundtrip_ok"`
	RoundtripTotal   int   `json:"roundtrip_total"`
	ForcedFlushes    int   `json:"forced_flushes"`
	FirstSeq         int64 `json:"first_seq"`
	LastSeq          int64 `json:"last_seq"`
}

// Consistency returns the byte-level roundtrip ratio in [0,1]; 1 when nothing
// was parsed.
func (s Stats) Consistency() float64 {
	if s.RoundtripTotal == 0 {
		return 1
	}
	return float64(s.RoundtripOK) / float64(s.RoundtripTotal)
}

// Repairer holds the bounded reorder buffer.
type Repairer struct {
	opts      Options
	file      string
	stats     Stats
	buf       map[uint32]*Packet
	next      uint32
	highest   uint32
	started   bool
	delivered map[uint32]bool
	lossRun   int
	lastLost  int64
	haveLoss  bool
	issues    []*media.Issue
}

// New creates a Repairer.
func New(opts Options, file string) *Repairer {
	return &Repairer{opts: opts.normalize(), file: file, buf: map[uint32]*Packet{},
		delivered: map[uint32]bool{}, lastLost: -1}
}

// Issues returns parsing problems collected so far.
func (r *Repairer) Issues() []*media.Issue { return r.issues }

// Stats returns a snapshot.
func (r *Repairer) Stats() Stats { return r.stats }

// Parse unpacks one datagram. A nil packet and an issue are returned when the
// datagram is malformed; parsing continues for later datagrams.
func (r *Repairer) Parse(raw []byte, offset int64, arrivalNS int64) (*Packet, error) {
	if len(raw) > r.opts.MaxPacketBytes {
		return r.bad(media.CodeLimitExceeded, offset, "packet is %d bytes, limit is %d", len(raw), r.opts.MaxPacketBytes)
	}
	var pkt rtp.Packet
	if err := pkt.Unmarshal(raw); err != nil {
		return r.bad(media.CodeMalformed, offset, "RTP header unreadable: %v", err)
	}
	clone := append([]byte(nil), raw...)
	out := &Packet{Raw: clone, Header: pkt.Header, Payload: append([]byte(nil), pkt.Payload...),
		SSRC: pkt.SSRC, Offset: offset, ArrivalNS: arrivalNS}
	r.stats.RoundtripTotal++
	if encoded, err := out.Marshal(); err == nil && bytes.Equal(encoded, clone) {
		r.stats.RoundtripOK++
	}
	return out, nil
}

func (r *Repairer) bad(code media.Code, offset int64, format string, args ...any) (*Packet, error) {
	r.stats.MalformedPackets++
	is := media.New(media.StageRTP, media.InputRTP, code, format, args...).WithFile(r.file).WithOffset(offset)
	r.issues = append(r.issues, is)
	return nil, is
}

// Marshal re-serialises the packet from its parsed representation.
func (p *Packet) Marshal() ([]byte, error) {
	out := rtp.Packet{Header: p.Header, Payload: p.Payload}
	buf := make([]byte, out.MarshalSize())
	n, err := out.MarshalTo(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// Push inserts one packet and returns every packet that is now contiguous.
// The returned slice is ordered by sequence number and may be empty.
func (r *Repairer) Push(p *Packet) []*Packet {
	if p == nil {
		return nil
	}
	r.stats.Received++
	ext := r.extend(p.Header.SequenceNumber)
	if !r.started {
		r.started = true
		r.next, r.highest = ext, ext
		r.stats.FirstSeq = int64(ext)
	}
	if r.delivered[ext] || r.buf[ext] != nil {
		r.stats.DuplicatePackets++
		r.dropDelivered(ext)
		return nil
	}
	if int64(ext) < int64(r.next) {
		r.stats.LatePackets++
		return nil
	}
	if ext < r.highest {
		// Arrived after a higher sequence number: this packet was reordered.
		r.stats.ReorderedPackets++
	}
	r.buf[ext] = p
	if len(r.buf) > r.stats.MaxBufferDepth {
		r.stats.MaxBufferDepth = len(r.buf)
	}
	if ext > r.highest {
		r.highest = ext
	}
	out := r.drain(false)
	r.stats.LastSeq = int64(r.highest)
	return out
}

// drain emits contiguous packets and resolves holes that exceed the reorder
// window or the buffer budget. It never waits on an outstanding packet once
// the window has been passed, so a lost packet cannot deadlock the stream.
func (r *Repairer) drain(force bool) []*Packet {
	var out []*Packet
	for {
		if p, ok := r.buf[r.next]; ok {
			delete(r.buf, r.next)
			r.markDelivered(r.next)
			r.closeLossRun()
			r.stats.Emitted++
			out = append(out, p)
			r.next++
			continue
		}
		if len(r.buf) == 0 {
			return out
		}
		minSeq := r.minSeq()
		if minSeq < r.next {
			// Should be unreachable; drop to guarantee progress.
			delete(r.buf, minSeq)
			continue
		}
		gap := minSeq - r.next
		overWindow := gap > uint32(r.opts.Window)
		overBudget := len(r.buf) >= r.opts.MaxBuffered
		if !force && !overWindow && !overBudget {
			return out
		}
		if !force {
			r.stats.ForcedFlushes++
		}
		for s := r.next; s < minSeq; s++ {
			r.lose(s)
		}
		r.next = minSeq
	}
}

// Flush drains the remainder, declaring every remaining hole lost. It always
// terminates and never waits for outstanding packets.
func (r *Repairer) Flush() []*Packet {
	out := r.drain(true)
	r.closeLossRun()
	r.stats.LastSeq = int64(r.highest)
	return out
}

func (r *Repairer) lose(seq uint32) {
	r.stats.LostPackets++
	if r.haveLoss && int64(seq) == r.lastLost+1 {
		r.lossRun++
	} else {
		r.closeLossRun()
		r.lossRun = 1
	}
	r.lastLost, r.haveLoss = int64(seq), true
}

func (r *Repairer) closeLossRun() {
	if r.lossRun >= 2 {
		r.stats.BurstLostPackets += r.lossRun
	}
	r.lossRun = 0
	r.haveLoss = false
}

func (r *Repairer) markDelivered(seq uint32) {
	r.delivered[seq] = true
	if len(r.delivered) > 4*r.opts.MaxBuffered {
		cutoff := int64(seq) - int64(r.opts.MaxBuffered)
		for k := range r.delivered {
			if int64(k) < cutoff {
				delete(r.delivered, k)
			}
		}
	}
}

func (r *Repairer) dropDelivered(seq uint32) {
	if int64(seq) < int64(r.next)-int64(r.opts.MaxBuffered) {
		delete(r.delivered, seq)
	}
}

// minSeq returns the smallest buffered sequence number.
func (r *Repairer) minSeq() uint32 {
	min := uint32(math.MaxUint32)
	for k := range r.buf {
		if k < min {
			min = k
		}
	}
	return min
}

// extend converts a 16-bit sequence number into an unwrapped uint32 using the
// highest sequence seen so far as the reference.
func (r *Repairer) extend(seq uint16) uint32 {
	if !r.started {
		return uint32(seq)
	}
	base := r.highest &^ 0xFFFF
	cand := base | uint32(seq)
	if cand+0x8000 < r.highest {
		cand += 0x10000
	} else if cand > r.highest+0x8000 && cand >= 0x10000 {
		cand -= 0x10000
	}
	return cand
}

// Summary formats the aggregate counters for terminal output.
func (s Stats) Summary() string {
	return fmt.Sprintf("received=%d emitted=%d dup=%d reordered=%d lost=%d burst_lost=%d",
		s.Received, s.Emitted, s.DuplicatePackets, s.ReorderedPackets, s.LostPackets, s.BurstLostPackets)
}
