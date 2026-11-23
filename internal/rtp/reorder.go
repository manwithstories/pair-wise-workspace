// Package rtp reorders RTP packets by sequence number, tolerating duplicates
// and distinguishing a merely late arrival from a real gap. A gap is held
// open for a configurable wait window during which a bounded number of
// retransmit attempts are made; once the window closes the gap is preserved
// as an explicit miss record instead of being silently dropped.
package rtp

import (
	"fmt"
	"sort"
)

// Packet is one RTP payload observation. Seq is the sequence number already
// extended past 16-bit wrap by the caller.
type Packet struct {
	Seq        uint64
	Timestamp  uint32
	Track      string
	Payload    []byte
	Retransmit bool
}

// Config bounds how long a gap may hold delivery back.
type Config struct {
	// Window is the maximum distance between the expected next sequence and
	// the highest observed sequence for a gap to stay open.
	Window uint64
	// MaxRetries is the number of retransmit attempts made per gap.
	MaxRetries int
	// MaxGapRun caps how many missing sequences one gap record may claim.
	MaxGapRun uint64
	// BufferBudget caps buffered out-of-order packets.
	BufferBudget int
}

// DefaultConfig suits an unstable venue feed: 128 sequences of slack, one
// retry, a 1024-sequence gap ceiling and room for 4096 buffered packets.
func DefaultConfig() Config {
	return Config{Window: 128, MaxRetries: 1, MaxGapRun: 1024, BufferBudget: 4096}
}

// RetryHook requests retransmission of a gap. Returning a packet whose Seq
// falls inside the gap injects it as a recovered observation; returning false
// means no retransmit was available.
type RetryHook func(gap Gap) (Packet, bool)

// Status classifies each pushed sequence number.
type Status uint8

const (
	StatusDelivered Status = iota + 1
	StatusBuffered
	StatusDuplicate
	StatusLate
	StatusGapRecovered
)

func (s Status) String() string {
	switch s {
	case StatusDelivered:
		return "delivered"
	case StatusBuffered:
		return "buffered"
	case StatusDuplicate:
		return "duplicate"
	case StatusLate:
		return "late"
	case StatusGapRecovered:
		return "gap-recovered"
	default:
		return "unknown"
	}
}

// Gap records a run of sequence numbers that never arrived in time.
type Gap struct {
	First     uint64 `json:"first"`
	Count     uint64 `json:"count"`
	Track     string `json:"track"`
	Recovered bool   `json:"recovered"`
	Retries   int    `json:"retries"`
}

// Stats counts every classification for reporting.
type Stats struct {
	Received    int
	Delivered   int
	Duplicates  int
	Late        int
	Recovered   int
	FinalGaps   int
	MissingSeqs uint64
	Overflows   int
}

// Reorderer turns a jumbled packet stream into an ordered one. It serves one
// track and is not safe for concurrent use.
type Reorderer struct {
	cfg   Config
	track string
	next  uint64
	high  uint64
	buf   map[uint64]Packet
	done  map[uint64]struct{}
	open  *Gap // currently observed gap starting at next, awaiting arrival
	gaps  []Gap
	Stats Stats
}

// NewReorderer starts a track at the manifest-declared first sequence.
func NewReorderer(track string, start uint64, cfg Config) *Reorderer {
	if cfg.Window == 0 || cfg.BufferBudget <= 0 || cfg.MaxGapRun == 0 {
		cfg = DefaultConfig()
	}
	if cfg.MaxRetries < 0 {
		cfg.MaxRetries = 0
	}
	return &Reorderer{
		cfg:   cfg,
		track: track,
		next:  start,
		buf:   map[uint64]Packet{},
		done:  map[uint64]struct{}{},
	}
}

// Push feeds one packet and returns the packets that became deliverable in
// order, the gap records that closed, and the classification of this packet.
func (r *Reorderer) Push(p Packet, hook RetryHook) (out []Packet, closed []Gap, st Status) {
	r.Stats.Received++
	if p.Track == "" {
		p.Track = r.track
	}
	if _, seen := r.done[p.Seq]; seen {
		r.Stats.Duplicates++
		return nil, nil, StatusDuplicate
	}
	if _, buffered := r.buf[p.Seq]; buffered {
		r.Stats.Duplicates++
		return nil, nil, StatusDuplicate
	}
	if p.Seq < r.next {
		r.Stats.Late++
		return nil, nil, StatusLate
	}
	if len(r.buf) >= r.cfg.BufferBudget {
		r.Stats.Overflows++
		// Budget exhausted: declare the gap immediately rather than grow.
		r.finalizeGap(minU64(r.gapLen(), r.cfg.MaxGapRun))
	}
	r.buf[p.Seq] = p
	if p.Seq > r.high {
		r.high = p.Seq
	}
	st = StatusBuffered
	out, closed = r.advance(hook)
	if len(out) > 0 {
		st = StatusDelivered
		for _, d := range out {
			if d.Retransmit {
				st = StatusGapRecovered
			}
		}
	}
	return out, closed, st
}

// advance delivers contiguous packets and resolves gaps whose wait window has
// closed. Each iteration either increases next or returns, so it terminates.
func (r *Reorderer) advance(hook RetryHook) (out []Packet, closed []Gap) {
	for {
		out, closed = r.drain(out, closed)
		if r.next > r.high {
			return out, closed
		}
		if _, ok := r.buf[r.next]; ok {
			continue // drain will consume it
		}
		if r.open == nil || r.open.First != r.next {
			r.open = &Gap{First: r.next, Track: r.track}
		}
		// Bounded retransmit attempts while the gap is open.
		for r.open != nil && r.open.Retries < r.cfg.MaxRetries && hook != nil {
			g := *r.open
			g.Count = r.gapLen()
			r.open.Retries++
			g.Retries = r.open.Retries
			pkt, ok := hook(g)
			if !ok {
				break
			}
			if pkt.Seq >= r.next && pkt.Seq <= r.high {
				if _, dup := r.buf[pkt.Seq]; !dup {
					if pkt.Track == "" {
						pkt.Track = r.track
					}
					pkt.Retransmit = true
					r.buf[pkt.Seq] = pkt
				}
			}
			if _, got := r.buf[r.next]; got {
				break
			}
		}
		if _, got := r.buf[r.next]; got {
			continue
		}
		if r.high-r.next < r.cfg.Window {
			return out, closed // still inside the wait window
		}
		// Window closed: finalize the gap and jump to the next buffered seq.
		n := minU64(r.gapLen(), r.cfg.MaxGapRun)
		closed = append(closed, r.finalizeGap(n))
	}
}

// drain emits every buffered packet contiguous with next.
func (r *Reorderer) drain(out []Packet, closed []Gap) ([]Packet, []Gap) {
	for {
		p, ok := r.buf[r.next]
		if !ok {
			return out, closed
		}
		delete(r.buf, r.next)
		r.done[r.next] = struct{}{}
		out = append(out, p)
		r.Stats.Delivered++
		r.next++
		if r.open != nil && r.open.First < r.next {
			// The missing sequence arrived before the window closed.
			g := *r.open
			g.Recovered = true
			g.Count = 1
			g.Retries = r.open.Retries
			r.Stats.Recovered++
			closed = append(closed, g)
			r.gaps = append(r.gaps, g)
			r.open = nil
		}
	}
}

// finalizeGap records a miss of n sequences at next and advances the cursor
// to the next buffered sequence (or past the high-water mark).
func (r *Reorderer) finalizeGap(n uint64) Gap {
	g := Gap{First: r.next, Count: n, Track: r.track, Recovered: false}
	if r.open != nil {
		g.Retries = r.open.Retries
	}
	r.Stats.FinalGaps++
	r.Stats.MissingSeqs += n
	r.gaps = append(r.gaps, g)
	r.open = nil
	if lowest, ok := r.lowestBuffered(); ok && lowest > r.next {
		r.next = lowest
	} else if !ok {
		r.next = r.high + 1
	}
	return g
}

// gapLen reports how many sequences are missing between next and the first
// buffered sequence (or the high-water mark when nothing is buffered).
func (r *Reorderer) gapLen() uint64 {
	if lowest, ok := r.lowestBuffered(); ok {
		if lowest > r.next {
			return lowest - r.next
		}
		return 1
	}
	if r.high >= r.next {
		return r.high - r.next + 1
	}
	return 1
}

func (r *Reorderer) lowestBuffered() (uint64, bool) {
	if len(r.buf) == 0 {
		return 0, false
	}
	var lowest uint64 = ^uint64(0)
	for seq := range r.buf {
		if seq < lowest {
			lowest = seq
		}
	}
	return lowest, true
}

// Close finalizes every open gap and drains the remaining buffer. It must be
// called exactly once at end of stream.
func (r *Reorderer) Close(hook RetryHook) ([]Packet, []Gap) {
	var out []Packet
	var closed []Gap
	for {
		out, closed = r.drain(out, closed)
		if r.next > r.high && len(r.buf) == 0 {
			break
		}
		if _, ok := r.buf[r.next]; !ok {
			if r.open == nil {
				r.open = &Gap{First: r.next, Track: r.track}
			}
			// One last retransmit attempt at end of stream.
			if r.open.Retries < r.cfg.MaxRetries && hook != nil {
				g := *r.open
				g.Count = r.gapLen()
				r.open.Retries++
				g.Retries = r.open.Retries
				if pkt, ok := hook(g); ok && pkt.Seq >= r.next && pkt.Seq <= r.high {
					if _, dup := r.buf[pkt.Seq]; !dup {
						if pkt.Track == "" {
							pkt.Track = r.track
						}
						pkt.Retransmit = true
						r.buf[pkt.Seq] = pkt
					}
				}
				continue
			}
			closed = append(closed, r.finalizeGap(minU64(r.gapLen(), r.cfg.MaxGapRun)))
			continue
		}
	}
	return out, closed
}

// Pending reports packets still held back by an open gap.
func (r *Reorderer) Pending() int { return len(r.buf) }

// FinalGaps returns every gap record accumulated so far, recovered and final.
func (r *Reorderer) FinalGaps() []Gap { return r.gaps }

func minU64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

// FormatGap renders a gap for terminal output and provenance logs.
func FormatGap(g Gap) string {
	return fmt.Sprintf("%s#%d+%d (recovered=%t,retries=%d)",
		g.Track, g.First, g.Count, g.Recovered, g.Retries)
}

// SortGaps orders gaps by track then first sequence.
func SortGaps(g []Gap) {
	sort.Slice(g, func(i, j int) bool {
		if g[i].Track != g[j].Track {
			return g[i].Track < g[j].Track
		}
		return g[i].First < g[j].First
	})
}
