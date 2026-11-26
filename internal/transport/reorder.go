// Package transport performs bounded reordering, de-duplication and loss
// accounting before ISO-BMFF parsing.
package transport

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pairwise/relay/internal/media"
)

// PacketID is the identity used for de-duplication. SourceNode is provenance,
// not identity: redundant copies from two edge nodes describe one packet.
type PacketID struct {
	Stream    string `json:"stream"`
	Rendition string `json:"rendition"`
	Sequence  uint64 `json:"sequence"`
}

func (id PacketID) key() string {
	return id.Stream + "\x00" + id.Rendition
}

func (id PacketID) String() string {
	return fmt.Sprintf("stream=%s rendition=%s seq=%d", id.Stream, id.Rendition, id.Sequence)
}

// Packet is one received fragment plus transport provenance.
type Packet struct {
	ID             PacketID
	SourceNode     string
	Data           []byte
	Bytes          int64
	Retransmission bool
}

// Gap records a sequence that could not be delivered in order.
type Gap struct {
	ID         PacketID
	Reason     media.Code
	Attempts   int
	Recovered  bool
	ByteOffset int64
}

// TrackStats contains per-track transport counters.
type TrackStats struct {
	Next        uint64
	Received    int
	Duplicates  int
	OutOfOrder  int
	Late        int
	Missing     int
	Recovered   int
	PendingSize int
}

// Stats aggregates transport observations.
type Stats struct {
	Packets    int
	Duplicates int
	OutOfOrder int
	Late       int
	Missing    int
	Recovered  int
	Gaps       []Gap
	Tracks     map[string]TrackStats
}

type trackState struct {
	next     uint64
	pending  map[uint64]Packet
	skipped  map[uint64]Gap
	stats    TrackStats
	firstSet bool
}

// Reorderer is a bounded per-track reordering window.
type Reorderer struct {
	window     uint64
	maxPending int
	firstSeq   uint64
	tracks     map[string]*trackState
	stats      Stats
}

// NewReorderer creates a reorderer. window is the maximum accepted distance
// ahead of the contiguous frontier (must be positive); maxPending bounds RAM.
func NewReorderer(window uint64, maxPending int, firstSeq uint64) (*Reorderer, error) {
	if window == 0 {
		return nil, media.NewFault(media.CodeInvalidArgument, media.FragmentID{}, -1, false, "reorder window must be positive")
	}
	if maxPending <= 0 || uint64(maxPending) < window {
		return nil, media.NewFault(media.CodeInvalidArgument, media.FragmentID{}, -1, false, "maxPending must be >= window and positive")
	}
	if firstSeq == 0 {
		firstSeq = 1
	}
	return &Reorderer{
		window:     window,
		maxPending: maxPending,
		firstSeq:   firstSeq,
		tracks:     make(map[string]*trackState),
		stats:      Stats{Tracks: make(map[string]TrackStats)},
	}, nil
}

// PushResult describes the effect of one pushed packet.
type PushResult struct {
	Accepted  bool
	Duplicate bool
	Late      bool
	Outside   bool
	Ordered   []Packet
	Fault     *media.Fault
}

// Push accepts one packet and returns any newly contiguous ordered packets.
func (r *Reorderer) Push(p Packet) PushResult {
	if p.ID.Sequence == 0 {
		return PushResult{Fault: media.NewFault(media.CodeInvalidArgument, fragmentID(p), -1, false, "packet sequence must be positive")}
	}
	if len(p.Data) == 0 && p.Bytes == 0 {
		return PushResult{Fault: media.NewFault(media.CodeInvalidArgument, fragmentID(p), -1, false, "empty packet")}
	}
	ts := r.track(p.ID.key())
	if !ts.firstSet {
		ts.next = r.firstSeq
		ts.firstSet = true
	}
	r.stats.Packets++
	ts.stats.Received++

	// Already advanced past this sequence. It is either a delivered
	// duplicate or a late copy of a gap that was explicitly skipped.
	if p.ID.Sequence < ts.next {
		if gap, ok := ts.skipped[p.ID.Sequence]; ok && !gap.Recovered {
			r.stats.Late++
			ts.stats.Late++
			f := media.NewFault(media.CodeLatePacket, fragmentID(p), -1, true, "packet arrived after gap was closed")
			return PushResult{Late: true, Fault: f}
		}
		r.stats.Duplicates++
		ts.stats.Duplicates++
		return PushResult{Duplicate: true}
	}

	if _, ok := ts.pending[p.ID.Sequence]; ok {
		r.stats.Duplicates++
		ts.stats.Duplicates++
		return PushResult{Duplicate: true}
	}

	if p.ID.Sequence >= ts.next+r.window {
		f := media.NewFault(media.CodeOutOfOrder, fragmentID(p), -1, true,
			"sequence %d is outside reorder window [frontier=%d, width=%d]", p.ID.Sequence, ts.next, r.window)
		return PushResult{Outside: true, Fault: f}
	}
	if len(ts.pending) >= r.maxPending {
		f := media.NewFault(media.CodeInputTooLarge, fragmentID(p), -1, false, "reorder queue limit %d reached", r.maxPending)
		return PushResult{Outside: true, Fault: f}
	}
	if p.ID.Sequence != ts.next {
		r.stats.OutOfOrder++
		ts.stats.OutOfOrder++
	}
	ts.pending[p.ID.Sequence] = p
	ordered := r.drain(p.ID.key(), ts)
	return PushResult{Accepted: true, Ordered: ordered}
}

// Missing returns un-delivered sequences in [first, through].
func (r *Reorderer) Missing(trackKey string, stream, rendition string, through uint64) []PacketID {
	ts := r.track(trackKey)
	if !ts.firstSet {
		ts.next = r.firstSeq
		ts.firstSet = true
	}
	var out []PacketID
	for seq := r.firstSeq; seq <= through; seq++ {
		if seq >= ts.next {
			if _, ok := ts.pending[seq]; !ok {
				out = append(out, PacketID{Stream: stream, Rendition: rendition, Sequence: seq})
			}
		}
	}
	return out
}

// Recover supplies a refetched packet before gaps are closed.
func (r *Reorderer) Recover(p Packet) ([]Packet, *media.Fault) {
	ts := r.track(p.ID.key())
	if gap, ok := ts.skipped[p.ID.Sequence]; ok {
		delete(ts.skipped, p.ID.Sequence)
		gap.Recovered = true
		ts.stats.Recovered++
		r.stats.Recovered++
	}
	res := r.Push(p)
	if res.Fault != nil && !res.Duplicate {
		return res.Ordered, res.Fault
	}
	return res.Ordered, nil
}

// CloseGap marks one still-missing sequence as an explicit gap and advances
// the contiguous frontier over it. Buffered following packets are then drained.
func (r *Reorderer) CloseGap(id PacketID, attempts int) []Packet {
	ts := r.track(id.key())
	if id.Sequence < ts.next {
		return nil
	}
	if _, ok := ts.pending[id.Sequence]; ok {
		return r.drain(id.key(), ts)
	}
	gap := Gap{ID: id, Reason: media.CodeRecoveryBudget, Attempts: attempts}
	ts.skipped[id.Sequence] = gap
	r.stats.Gaps = append(r.stats.Gaps, gap)
	r.stats.Missing++
	ts.stats.Missing++
	if id.Sequence == ts.next {
		ts.next++
		return r.drain(id.key(), ts)
	}
	return nil
}

// OrderedSnapshot returns pending packets sorted by sequence without changing
// frontier state; useful for diagnostics and tests.
func (r *Reorderer) OrderedSnapshot(trackKey string) []Packet {
	ts := r.track(trackKey)
	seqs := make([]uint64, 0, len(ts.pending))
	for seq := range ts.pending {
		seqs = append(seqs, seq)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	out := make([]Packet, 0, len(seqs))
	for _, seq := range seqs {
		out = append(out, ts.pending[seq])
	}
	return out
}

// Stats returns a copy of aggregate statistics.
func (r *Reorderer) Stats() Stats {
	out := r.stats
	out.Tracks = make(map[string]TrackStats, len(r.tracks))
	for k, ts := range r.tracks {
		ts.stats.Next = ts.next
		ts.stats.PendingSize = len(ts.pending)
		out.Tracks[k] = ts.stats
	}
	return out
}

func (r *Reorderer) track(key string) *trackState {
	ts, ok := r.tracks[key]
	if !ok {
		ts = &trackState{next: r.firstSeq, pending: make(map[uint64]Packet), skipped: make(map[uint64]Gap)}
		r.tracks[key] = ts
	}
	return ts
}

func (r *Reorderer) drain(key string, ts *trackState) []Packet {
	var out []Packet
	for {
		p, ok := ts.pending[ts.next]
		if !ok {
			break
		}
		out = append(out, p)
		delete(ts.pending, ts.next)
		ts.next++
	}
	return out
}

func fragmentID(p Packet) media.FragmentID {
	return media.FragmentID{Rendition: p.ID.Rendition, Segment: p.ID.Sequence}
}

// SafeTrackName makes a filesystem/playlist-safe rendition label.
func SafeTrackName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "..", "_")
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, string('\x00'), "_")
	return s
}
