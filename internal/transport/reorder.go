package transport

import (
	"fmt"
	"sort"

	"mediaflow/internal/fixture"
	"mediaflow/internal/timeline"
)

// Stats summarizes one transport recovery pass. Counters are cumulative.
type Stats struct {
	Ingested   int // all packets offered (media + parity, incl. duplicates)
	Duplicates int // collapsed duplicate arrivals
	Reordered  int // packets that arrived after a later sequence
	Recovered  int // media packets rebuilt from parity
	LostMedia  int // media slots that never arrived
	Gaps       int // unrecovered gap records emitted
	Delivered  int // media packets handed downstream (arrived + recovered)
	Wraps      int // modulus rollovers expanded
	Backwards  int // genuine non-wrap backwards discontinuities
}

// Config tunes the per-track receiver.
type Config struct {
	// ReorderWindow bounds how long the receiver waits for a late packet
	// before declaring the missing sequences a loss.
	ReorderWindow int
	// WrapModulus > 0 enables per-track sequence unwrapping (e.g. 1<<16 RTP).
	WrapModulus int64
}

// trackReceiver holds per-track reorder and FEC state.
type trackReceiver struct {
	track    uint32
	unwrap   *timeline.Unwrapper
	expected int64 // next expanded media sequence to deliver
	arrived  map[int64]fixture.Packet
	seen     map[int64]bool
	fec      *FECResolver
	maxSeen  int64
}

// Receiver restores all tracks of one stream.
type Receiver struct {
	cfg    Config
	tracks map[uint32]*trackReceiver
	stats  Stats
	out    []fixture.Packet
	gaps   []Gap
}

// NewReceiver validates the config and builds an empty receiver.
func NewReceiver(cfg Config) (*Receiver, error) {
	if cfg.ReorderWindow < 1 {
		return nil, timeline.GlobalError("transport.new", false,
			errf("reorder window must be >= 1, got %d", cfg.ReorderWindow))
	}
	if cfg.WrapModulus > 0 {
		if _, err := timeline.NewUnwrapper(cfg.WrapModulus); err != nil {
			return nil, timeline.GlobalError("transport.new", false, err)
		}
	}
	return &Receiver{
		cfg:    cfg,
		tracks: map[uint32]*trackReceiver{},
	}, nil
}

func (r *Receiver) trackState(track uint32) *trackReceiver {
	ts, ok := r.tracks[track]
	if !ok {
		var unw *timeline.Unwrapper
		if r.cfg.WrapModulus > 0 {
			unw, _ = timeline.NewUnwrapper(r.cfg.WrapModulus)
		}
		ts = &trackReceiver{
			track:   track,
			unwrap:  unw,
			arrived: map[int64]fixture.Packet{},
			seen:    map[int64]bool{},
			fec:     NewFECResolver(track),
		}
		r.tracks[track] = ts
	}
	return ts
}

// Ingest accepts one delivered packet (media or parity).
func (r *Receiver) Ingest(p fixture.Packet) error {
	r.stats.Ingested++
	ts := r.trackState(p.Track)
	if p.IsParity {
		ts.fec.AddParity(p)
		return r.drain(ts)
	}

	expanded := int64(p.Seq)
	if ts.unwrap != nil {
		v, err := ts.unwrap.Expand(p.Track, int64(p.Seq), timeline.RationalOne)
		if err != nil {
			if te, ok := timeline.AsError(err); ok {
				if te.Is(timeline.ErrWrap) && ts.unwrap.Backwards() > r.stats.Backwards {
					r.stats.Backwards = ts.unwrap.Backwards()
				}
			}
		}
		expanded = v
		r.stats.Wraps = ts.unwrap.Wraps()
	}

	if ts.seen[expanded] {
		r.stats.Duplicates++
		return nil
	}
	ts.seen[expanded] = true
	if expanded > ts.maxSeen {
		ts.maxSeen = expanded
	}
	if expanded < ts.expected {
		// Already flushed region: a true duplicate/late loss, do not re-deliver.
		return nil
	}
	if _, exists := ts.arrived[expanded]; !exists && expanded != ts.expected {
		r.stats.Reordered++
	}
	ts.arrived[expanded] = p
	r.flushAvailable(ts)
	r.expireWindow(ts)
	return r.drain(ts)
}

// flushAvailable delivers every contiguous sequence starting at expected.
func (r *Receiver) flushAvailable(ts *trackReceiver) {
	for {
		p, ok := ts.arrived[ts.expected]
		if !ok {
			return
		}
		delete(ts.arrived, ts.expected)
		slot := int(ts.expected % fixture.FECGroupSize)
		ts.fec.AddMedia(p, slot)
		ts.expected++
	}
}

// expireWindow declares sequences lost once the highest seen sequence has
// advanced more than ReorderWindow past the expected one: any slot still
// missing inside that span can no longer plausibly arrive. Watermark-based
// (not buffer-length-based) so losses actually trigger expiry.
func (r *Receiver) expireWindow(ts *trackReceiver) {
	for ts.maxSeen-ts.expected >= int64(r.cfg.ReorderWindow) {
		if _, ok := ts.arrived[ts.expected]; ok {
			r.flushAvailable(ts)
			continue
		}
		group := int(ts.expected / fixture.FECGroupSize)
		slot := int(ts.expected % fixture.FECGroupSize)
		ts.fec.MarkMissing(group, slot)
		r.stats.LostMedia++
		ts.expected++
	}
}

func (ts *trackReceiver) minBuffered() int64 {
	min := ts.maxSeen + 1
	for seq := range ts.arrived {
		if seq < min {
			min = seq
		}
	}
	return min
}

// drain pulls finalized FEC groups into the output and gap lists.
func (r *Receiver) drain(ts *trackReceiver) error {
	pkts, gaps := ts.fec.Drain()
	for _, p := range pkts {
		if p.Recovered {
			// LostMedia was already counted when the slot was marked missing.
			r.stats.Recovered++
		}
		r.stats.Delivered++
		r.out = append(r.out, p)
	}
	r.stats.Gaps += len(gaps)
	r.gaps = append(r.gaps, gaps...)
	return nil
}

// Flush ends the stream: remaining buffered gaps are declared and all FEC
// groups forced to resolution. No media data is invented for open losses.
func (r *Receiver) Flush() ([]fixture.Packet, []Gap, Stats) {
	for _, ts := range r.tracks {
		// Mark every sequence between expected and maxSeen that never arrived.
		for seq := ts.expected; seq <= ts.maxSeen; seq++ {
			if _, ok := ts.arrived[seq]; ok {
				continue
			}
			group := int(seq / fixture.FECGroupSize)
			slot := int(seq % fixture.FECGroupSize)
			ts.fec.MarkMissing(group, slot)
			r.stats.LostMedia++
			ts.expected = seq + 1
		}
		// Drop anything still buffered after gap marking.
		r.flushAvailable(ts)
		pkts, gaps := ts.fec.Flush()
		for _, p := range pkts {
			if p.Recovered {
				r.stats.Recovered++
			}
			r.stats.Delivered++
			r.out = append(r.out, p)
		}
		r.stats.Gaps += len(gaps)
		r.gaps = append(r.gaps, gaps...)
	}
	sort.SliceStable(r.out, func(i, j int) bool {
		if r.out[i].TrueUS != r.out[j].TrueUS {
			return r.out[i].TrueUS < r.out[j].TrueUS
		}
		return r.out[i].Track < r.out[j].Track
	})
	return r.out, r.gaps, r.stats
}

// Gaps returns the gaps emitted so far.
func (r *Receiver) Gaps() []Gap { return append([]Gap(nil), r.gaps...) }

// Stats returns a copy of the cumulative counters.
func (r *Receiver) Stats() Stats { return r.stats }

func errf(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}
