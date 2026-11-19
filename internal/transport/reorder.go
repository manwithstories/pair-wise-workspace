// Package transport ingests RTP packets, removes duplicates, restores sequence
// order across 16-bit wraparound and tracks gaps for later recovery.
package transport

import (
	"hash/fnv"
	"sort"

	"mediamix/internal/core"
	"mediamix/internal/timeline"
)

// Limits bounds a single track's reorder state.
type Limits struct {
	MaxBuffered int
	MaxTracks   int
}

// DefaultLimits are deliberately small: a proxy stream must not pin unbounded
// memory while a weak uplink is being repaired.
func DefaultLimits() Limits { return Limits{MaxBuffered: 2048, MaxTracks: 64} }

// Packet is one depacketized RTP packet on the shared media timeline.
type Packet struct {
	Track      string
	SSRC       uint32
	Seq        uint16
	Ext        uint64
	Timestamp  uint32
	Marker     bool
	Keyframe   bool
	Payload    []byte
	ArrivalUs  timeline.Micros
	Retransmit bool
}

// digest identifies payload identity for duplicate-conflict detection.
func (p Packet) digest() uint64 {
	h := fnv.New64a()
	var hdr [8]byte
	for i := 0; i < 4; i++ {
		hdr[i] = byte(p.Timestamp >> (8 * (3 - i)))
	}
	hdr[4] = byte(p.Seq >> 8)
	hdr[5] = byte(p.Seq)
	if p.Marker {
		hdr[6] = 1
	}
	if p.Keyframe {
		hdr[7] = 1
	}
	_, _ = h.Write(hdr[:])
	_, _ = h.Write(p.Payload)
	return h.Sum64()
}

// Stats accumulates per-track transport counters.
type Stats struct {
	Received     int `json:"received"`
	Delivered    int `json:"delivered"`
	Duplicates   int `json:"duplicates"`
	Conflicts    int `json:"conflicts"`
	Late         int `json:"late"`
	Reordered    int `json:"reordered"`
	Recovered    int `json:"recovered"`
	PeakBuffered int `json:"peak_buffered"`
}

// Gap describes a contiguous run of missing extended sequence numbers.
type Gap struct {
	Track    string          `json:"track"`
	StartExt uint64          `json:"start_ext"`
	EndExt   uint64          `json:"end_ext"` // inclusive
	Count    int             `json:"count"`
	SinceUs  timeline.Micros `json:"since_us"`
}

// ReorderBuffer reorders one track's packets and reports gaps instead of
// blocking indefinitely.
type ReorderBuffer struct {
	track    string
	limits   Limits
	ext      *Extender
	expected uint64
	started  bool
	buf      map[uint64]Packet
	digests  map[uint64]uint64
	maxSeen  uint64
	gaps     map[uint64]Gap
	stats    Stats
	closed   bool
}

// NewReorderBuffer creates the per-track buffer.
func NewReorderBuffer(track string, limits Limits) (*ReorderBuffer, error) {
	if track == "" {
		return nil, &core.Error{Code: core.CodeBadArgument, Detail: "empty track id", Fatal: true}
	}
	if limits.MaxBuffered <= 0 {
		limits = DefaultLimits()
	}
	return &ReorderBuffer{
		track:   track,
		limits:  limits,
		ext:     NewExtender(),
		buf:     make(map[uint64]Packet),
		digests: make(map[uint64]uint64),
		gaps:    make(map[uint64]Gap),
	}, nil
}

// Track returns the buffer's track id.
func (r *ReorderBuffer) Track() string { return r.track }

// Stats returns a snapshot of counters.
func (r *ReorderBuffer) Stats() Stats { return r.stats }

// Expected returns the next extended sequence number that will be delivered.
func (r *ReorderBuffer) Expected() uint64 { return r.expected }

// Buffered returns the number of packets held back by a gap.
func (r *ReorderBuffer) Buffered() int { return len(r.buf) }

// OpenGaps returns gaps sorted by sequence number.
func (r *ReorderBuffer) OpenGaps() []Gap {
	out := make([]Gap, 0, len(r.gaps))
	for _, g := range r.gaps {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartExt < out[j].StartExt })
	return out
}

// Push accepts a packet and returns, in order, every packet that became
// deliverable. A duplicate never re-enters the buffer; a conflicting duplicate
// keeps the first copy and is counted separately.
func (r *ReorderBuffer) Push(p Packet, now timeline.Micros) ([]Packet, error) {
	if r.closed {
		return nil, &core.Error{Code: core.CodeBadPacket, Track: r.track, Detail: "buffer closed", Fatal: true}
	}
	if p.Track == "" {
		p.Track = r.track
	}
	if p.Track != r.track {
		return nil, core.SeqError(core.CodeBadPacket, r.track, uint32(p.Seq), "packet routed to wrong track buffer")
	}
	if p.ArrivalUs < 0 || len(p.Payload) == 0 {
		return nil, core.SeqError(core.CodeBadPacket, r.track, uint32(p.Seq), "empty payload or negative arrival")
	}
	ext, err := r.ext.Extend(r.track, p.Seq)
	if err != nil {
		return nil, core.StampTrack(err, r.track)
	}
	p.Ext = ext
	if !r.started {
		r.started = true
		r.expected = ext
	}
	r.stats.Received++
	if d, seen := r.digests[ext]; seen {
		if d != p.digest() {
			r.stats.Conflicts++
			return nil, core.SeqError(core.CodeBadPacket, r.track, uint32(p.Seq),
				"duplicate sequence with conflicting payload")
		}
		r.stats.Duplicates++
		return nil, nil
	}
	if ext < r.expected {
		// The cursor already passed this sequence: late arrival.
		r.stats.Late++
		r.stats.Reordered++
		r.digests[ext] = p.digest()
		return []Packet{p}, nil
	}
	if ext > r.maxSeen {
		r.maxSeen = ext
	} else {
		r.stats.Reordered++
	}
	if _, defeated := r.gaps[ext]; defeated {
		delete(r.gaps, ext)
	}
	r.buf[ext] = p
	r.digests[ext] = p.digest()
	r.pruneDigests()
	if len(r.buf) > r.limits.MaxBuffered {
		return nil, &core.Error{Code: core.CodeOverLimit, Track: r.track,
			Detail: "reorder buffer limit exceeded", Fatal: false}
	}
	r.stats.PeakBuffered = maxInt(r.stats.PeakBuffered, len(r.buf))
	out := r.drain()
	r.trackGap(now)
	return out, nil
}

// drain emits the contiguous run starting at expected.
func (r *ReorderBuffer) drain() []Packet {
	var out []Packet
	for {
		p, ok := r.buf[r.expected]
		if !ok {
			break
		}
		delete(r.buf, r.expected)
		out = append(out, p)
		r.expected++
	}
	r.stats.Delivered += len(out)
	return out
}

// pruneDigests bounds duplicate-detection memory: once a packet is safely
// behind the cursor by more than the buffer limit, a duplicate of it can only
// be a late retransmission, which is still counted as late rather than
// reordered. Keeping recent history is enough to catch common dupes.
func (r *ReorderBuffer) pruneDigests() {
	limit := r.limits.MaxBuffered * 4
	if len(r.digests) <= limit {
		return
	}
	cutoff := uint64(0)
	if r.expected > uint64(limit) {
		cutoff = r.expected - uint64(limit)
	}
	for ext := range r.digests {
		if ext < cutoff {
			delete(r.digests, ext)
		}
	}
}

// trackGap records the hole revealed once packets are buffered beyond the
// cursor. The gap start stays stable until the hole is filled or flushed.
func (r *ReorderBuffer) trackGap(now timeline.Micros) {
	if len(r.buf) == 0 {
		return
	}
	first := uint64(0)
	found := false
	for ext := range r.buf {
		if ext > r.expected && (!found || ext < first) {
			first = ext
			found = true
		}
	}
	if !found {
		return
	}
	if g, ok := r.gaps[r.expected]; ok {
		g.EndExt = first - 1
		g.Count = int(first - r.expected)
		r.gaps[r.expected] = g
		return
	}
	r.gaps[r.expected] = Gap{
		Track:    r.track,
		StartExt: r.expected,
		EndExt:   first - 1,
		Count:    int(first - r.expected),
		SinceUs:  now,
	}
}

// Flush forces delivery of every buffered packet in sequence order, skipping
// remaining holes, and returns the packets plus the gaps that were skipped.
// This is how a deadline is enforced: buffering never grows without bound.
func (r *ReorderBuffer) Flush() ([]Packet, []Gap) {
	out := r.drain()
	skipped := r.OpenGaps()
	for _, g := range skipped {
		delete(r.gaps, g.StartExt)
	}
	if len(r.buf) > 0 {
		exts := make([]uint64, 0, len(r.buf))
		for ext := range r.buf {
			exts = append(exts, ext)
		}
		sort.Slice(exts, func(i, j int) bool { return exts[i] < exts[j] })
		r.expected = exts[0]
		out = append(out, r.drain()...)
	}
	r.closed = true
	return out, skipped
}

// Recovered marks a retransmitted packet as delivered at its position. The
// caller has already validated the recovery source.
func (r *ReorderBuffer) Recovered(p Packet) []Packet {
	if p.Ext == 0 {
		p.Ext = r.expected
	}
	r.stats.Delivered++
	r.stats.Reordered++
	r.digests[p.Ext] = p.digest()
	delete(r.gaps, p.Ext)
	return []Packet{p}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
