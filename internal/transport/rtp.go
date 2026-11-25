// Package transport recovers a well-ordered RTP stream from a lossy one. It
// detects duplicates and reordering, counts losses, and resolves 32-bit
// timestamp wraps through the shared timebase.
package transport

import (
	"sort"

	"relay.local/relay/internal/fault"
	"relay.local/relay/internal/model"
)

// Stats records what recovery observed.
type Stats struct {
	Received   int // packets supplied
	Unique     int // distinct sequence numbers accepted
	Duplicates int // repeat sequence numbers discarded
	Reordered  int // packets that arrived after a higher sequence number
	Lost       int // sequence numbers never received inside the window
	Dropped    int // packets beyond a full reorder window, deterministically dropped
	Wraps      int // 32-bit timestamp wraps expanded
}

// Packet is a minimal RTP packet independent of the wire encoding.
type Packet struct {
	Seq       uint16
	Timestamp uint32
	Marker    bool
	TrackID   uint32
	Payload   []byte
	// Arrival is the deterministic arrival order used for tie-breaking. A
	// zero value is replaced by the packet's position in the input slice.
	Arrival int
}

// Gap records a deterministic hole left in the output.
type Gap struct {
	AfterSeq uint16
	Count    int
	Reason   string
}

// Result is the recovered stream.
type Result struct {
	Packets []Packet
	Stats   Stats
	Gaps    []Gap
	Ticks   []int64 // unwrapped 90 kHz timestamp per output packet
	Epochs  []int64 // wrap epoch per output packet
	Faults  *fault.List
}

// Options bounds recovery work.
type Options struct {
	// Window is the reorder buffer capacity in sequence numbers. Zero uses 64.
	Window int
	// MaxPackets caps accepted input packets.
	MaxPackets int
	// MaxPayload caps a single payload size.
	MaxPayload int
}

func (o Options) normalized() Options {
	if o.Window <= 0 {
		o.Window = 64
	}
	if o.MaxPackets <= 0 {
		o.MaxPackets = 1 << 20
	}
	if o.MaxPayload <= 0 {
		o.MaxPayload = 1 << 20
	}
	return o
}

// Recover reorders and deduplicates packets deterministically.
//
// Recovery is a pure function of the input: packets are deduplicated by
// sequence number (earliest arrival wins), sequence numbers are unwrapped
// across the 16-bit rollover starting from the first arrival, and the result is
// sorted by absolute sequence. The bounded window is then applied to that
// ordered stream: whenever the distance from the last emitted sequence number
// exceeds Window, the packets inside the hole are declared dropped and a gap is
// recorded. Modelling the buffer this way keeps the outcome independent of
// timing while still bounding memory.
func Recover(in []Packet, opts Options) Result {
	opts = opts.normalized()
	res := Result{Faults: &fault.List{}}
	stats := Stats{Received: len(in)}

	seen := make(map[uint16]Packet, len(in))
	unique := make([]Packet, 0, len(in))
	for i, p := range in {
		cur := p
		if cur.Arrival == 0 {
			cur.Arrival = i + 1
		}
		if len(cur.Payload) > opts.MaxPayload {
			res.Faults.Add(fault.New(-1, 0, fault.KindLimit, false,
				"packet %d payload %d exceeds limit %d", cur.Seq, len(cur.Payload), opts.MaxPayload))
			continue
		}
		if prev, ok := seen[cur.Seq]; ok {
			stats.Duplicates++
			if cur.Arrival < prev.Arrival {
				seen[cur.Seq] = cur
			}
			continue
		}
		seen[cur.Seq] = cur
		unique = append(unique, cur)
		if len(unique) > opts.MaxPackets {
			res.Faults.Add(fault.New(-1, 0, fault.KindLimit, false,
				"packet count exceeds limit %d", opts.MaxPackets))
			break
		}
	}
	stats.Unique = len(unique)
	if len(unique) == 0 {
		res.Stats = stats
		return res
	}

	// Anchor the unwrap on the earliest-arriving packet so a stream that
	// starts close to the rollover is handled without an arbitrary epoch.
	anchor := unique[0]
	for _, p := range unique[1:] {
		if p.Arrival < anchor.Arrival {
			anchor = p
		}
	}
	abs := func(seq uint16) int64 {
		d := int64(uint16(seq - anchor.Seq))
		if d > 0x7fff {
			d -= 0x10000
		}
		return d // relative to anchor; negative only for genuinely late packets
	}
	// Reordering: compare each packet's arrival against the furthest sequence
	// seen in arrival order.
	byArrival := append([]Packet(nil), unique...)
	sort.SliceStable(byArrival, func(i, j int) bool { return byArrival[i].Arrival < byArrival[j].Arrival })
	var furthest int64 = -1 << 62
	for _, p := range byArrival {
		a := abs(p.Seq)
		if a < furthest {
			stats.Reordered++
		} else {
			furthest = a
		}
	}

	sort.SliceStable(unique, func(i, j int) bool {
		if ai, aj := abs(unique[i].Seq), abs(unique[j].Seq); ai != aj {
			return ai < aj
		}
		return unique[i].Arrival < unique[j].Arrival
	})

	// Apply the window over adjacent absolute positions.
	out := make([]Packet, 0, len(unique))
	var prevAbs int64
	for i, p := range unique {
		cur := abs(p.Seq)
		if i > 0 {
			gap := cur - prevAbs - 1
			if gap > 0 {
				if gap > int64(opts.Window) {
					stats.Dropped += int(gap)
					res.Gaps = append(res.Gaps, Gap{
						AfterSeq: unique[i-1].Seq, Count: int(gap), Reason: "reorder window full",
					})
				} else {
					stats.Lost += int(gap)
					res.Gaps = append(res.Gaps, Gap{
						AfterSeq: unique[i-1].Seq, Count: int(gap), Reason: "sequence gap",
					})
				}
			}
		}
		out = append(out, p)
		prevAbs = cur
	}
	res.Packets = out

	raws := make([]uint32, len(out))
	for i, p := range out {
		raws[i] = p.Timestamp
	}
	ticks, epochs, err := model.UnwrapSequence(raws, 0)
	if err != nil {
		res.Faults.Add(fault.Wrap(-1, 0, fault.KindOutOfRange, false, err))
	} else {
		res.Ticks = ticks
		res.Epochs = epochs
		stats.Wraps = model.CountWraps(epochs)
	}
	res.Stats = stats
	return res
}

// Reassembler collects payload fragments and emits whole access units.
type Reassembler struct {
	buf []byte
}

// Push appends a payload; a packet marked Marker completes an access unit.
// The returned unit is nil until a marker arrives.
func (r *Reassembler) Push(p Packet) []byte {
	r.buf = append(r.buf, p.Payload...)
	if !p.Marker {
		return nil
	}
	unit := append([]byte(nil), r.buf...)
	r.buf = r.buf[:0]
	return unit
}

// Pending returns the number of buffered bytes.
func (r *Reassembler) Pending() int { return len(r.buf) }
