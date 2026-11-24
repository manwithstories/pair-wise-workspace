package transport

import (
	"sort"

	"github.com/pion/rtp"
)

// Stats is the deterministic accounting for one RTP stream.
type Stats struct {
	PacketsSent              int     `json:"packets_sent"`
	PacketsReceived          int     `json:"packets_received"`
	UniquePackets            int     `json:"unique_packets"`
	Delivered                int     `json:"delivered"`
	Duplicates               int     `json:"duplicates"`
	Reordered                int     `json:"reordered"`
	Recovered                int     `json:"recovered"`
	Lost                     int     `json:"lost"`
	Unrecoverable            int     `json:"unrecoverable"`
	UnrecoverableOutOfWindow int     `json:"unrecoverable_out_of_window"`
	TimestampRegressions     int     `json:"timestamp_regressions"`
	RecoveryRate             float64 `json:"recovery_rate"`
	JitterMS                 float64 `json:"jitter_ms"`
	WindowSize               int     `json:"reorder_window"`
	OutOfWindow              int     `json:"out_of_window"`
}

// Receiver reassembles one RTP stream within a bounded reorder window. It
// records only sequence numbers and timing, never packet payload copies, so
// memory stays bounded by the window.
type Receiver struct {
	window    *ReorderWindow
	stats     Stats
	arrivals  map[uint16]int
	delivered map[uint16]bool
	lateOnly  map[uint16]bool
	high      uint16
	haveHigh  bool
	lastTs    uint32
	haveTs    bool
	jitter    float64
	prevArr   int64
	prevRtp   int64
	havePrev  bool
	clockRate int64
}

// NewReceiver returns a receiver with a bounded reorder window and RTP clock
// rate (ticks per second).
func NewReceiver(windowSize int, clockRate int64) *Receiver {
	if clockRate <= 0 {
		clockRate = 90000
	}
	return &Receiver{
		window:    NewReorderWindow(windowSize),
		arrivals:  make(map[uint16]int),
		delivered: make(map[uint16]bool),
		lateOnly:  make(map[uint16]bool),
		clockRate: clockRate,
	}
}

// Push feeds one arrival. Packets that become deliverable in order are
// returned. Duplicate and out-of-window packets are counted but not returned.
func (r *Receiver) Push(pkt *rtp.Packet, arrivalUs int64) []*rtp.Packet {
	if pkt == nil {
		return nil
	}
	seq := pkt.SequenceNumber
	r.stats.PacketsReceived++
	r.stats.UniquePackets++
	r.arrivals[seq]++
	if r.arrivals[seq] > 1 {
		r.stats.Duplicates++
		return nil
	}

	// Reordered means this sequence is behind the highest already seen. Such a
	// packet can still be recovered if it fits inside the window.
	wasBehind := r.haveHigh && seqLess(seq, r.high)
	if wasBehind {
		r.stats.Reordered++
	}
	if !r.haveHigh || seqLess(r.high, seq) {
		r.high = seq
		r.haveHigh = true
	}

	r.trackTiming(pkt, arrivalUs)

	ready, accepted := r.window.Push(pkt)
	if !accepted {
		// The packet arrived, but the window had already advanced past its
		// position; record it so the summary can report an out-of-window
		// unrecoverable count rather than lumping it in with plain loss.
		if wasBehind {
			r.lateOnly[seq] = true
		}
		return nil
	}
	delivered := false
	for _, p := range ready {
		r.delivered[p.SequenceNumber] = true
		if p.SequenceNumber == seq {
			delivered = true
		}
	}
	if wasBehind && delivered {
		r.stats.Recovered++
	}
	return ready
}

func (r *Receiver) trackTiming(pkt *rtp.Packet, arrivalUs int64) {
	rtpTs := int64(pkt.Timestamp)
	if r.haveTs && rtpTs < r.prevRtp {
		r.stats.TimestampRegressions++
	}
	if r.havePrev {
		expected := int64(float64(arrivalUs-r.prevArr) * float64(r.clockRate) / 1_000_000)
		d := expected - (rtpTs - r.prevRtp)
		if d < 0 {
			d = -d
		}
		r.jitter += (float64(d) - r.jitter) / 16
	}
	r.prevArr = arrivalUs
	r.prevRtp = rtpTs
	r.havePrev = true
	r.haveTs = true
	r.lastTs = pkt.Timestamp
}

// Finish flushes the reorder window, delivers what remains buffered, and
// computes loss statistics against the known sent range. sentSeq0 and
// sentCount describe the packets the sender emitted; the receiver classifies
// every one of them.
func (r *Receiver) Finish(sentSeq0 uint16, sentCount int) Stats {
	for _, p := range r.window.Flush() {
		r.delivered[p.SequenceNumber] = true
	}
	r.stats.PacketsSent = sentCount
	r.stats.OutOfWindow = r.window.HorizonJumps()
	for i := 0; i < sentCount; i++ {
		seq := sentSeq0 + uint16(i)
		switch {
		case r.delivered[seq]:
			r.stats.Delivered++
		case r.arrivals[seq] > 0:
			r.stats.Unrecoverable++
			if r.lateOnly[seq] {
				r.stats.UnrecoverableOutOfWindow++
			}
		default:
			r.stats.Lost++
		}
	}
	if sentCount > 0 {
		r.stats.RecoveryRate = float64(r.stats.Delivered) / float64(sentCount)
	} else {
		r.stats.RecoveryRate = 1
	}
	r.stats.JitterMS = r.jitter * 1000 / float64(r.clockRate)
	r.stats.WindowSize = r.window.size
	return r.stats
}

// Stats returns a snapshot of the current counters (not final).
func (r *Receiver) Stats() Stats {
	s := r.stats
	s.WindowSize = r.window.size
	return s
}

// Delivered returns delivered sequence numbers sorted on the 16-bit ring.
func (r *Receiver) Delivered() []uint16 {
	seqs := make([]uint16, 0, len(r.delivered))
	for s := range r.delivered {
		seqs = append(seqs, s)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqLess(seqs[i], seqs[j]) })
	return seqs
}

// DeliveredSet exposes the delivered sequence set for media-window mapping.
func (r *Receiver) DeliveredSet() map[uint16]bool {
	out := make(map[uint16]bool, len(r.delivered))
	for s := range r.delivered {
		out[s] = true
	}
	return out
}
