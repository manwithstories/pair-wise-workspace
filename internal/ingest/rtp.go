// Package ingest parses RTP packets and RTCP clock reports into
// track-scoped, extended-sequence packet records.
package ingest

import (
	"fmt"

	"github.com/pion/rtp"

	"mediad/internal/media"
)

// TrackParams describes one incoming track as learned from signaling.
type TrackParams struct {
	SSRC      uint32
	ClockRate int64  // RTP clock ticks per second
	Channels  int    // audio channel count, 0 for non-audio
	Layout    string // "mono", "stereo", "5.1"; "" means unconfirmed
	IsVideo   bool
}

// Packet is a parsed RTP packet with wrap-safe extended sequence number
// and timestamp.
type Packet struct {
	Track   uint32
	Seq     uint16
	ExtSeq  uint64
	TS      uint32
	ExtTS   int64
	PT      uint8
	Marker  bool
	Payload []byte
	At      int64 // input arrival index
}

// Track holds the per-SSRC reconstruction state.
type Track struct {
	Params  TrackParams
	started bool
	maxSeq  uint16
	seqROC  uint64
	maxTS   uint32
	tsROC   int64
}

// TrackSnapshot is the serializable part of Track state.
type TrackSnapshot struct {
	Started bool
	MaxSeq  uint16
	SeqROC  uint64
	MaxTS   uint32
	TSROC   int64
}

func (t *Track) Snapshot() TrackSnapshot {
	return TrackSnapshot{t.started, t.maxSeq, t.seqROC, t.maxTS, t.tsROC}
}

// Restore replaces the reconstruction state, e.g. after a checkpoint load.
func (t *Track) Restore(s TrackSnapshot) {
	t.started, t.maxSeq, t.seqROC, t.maxTS, t.tsROC = s.Started, s.MaxSeq, s.SeqROC, s.MaxTS, s.TSROC
}

// extendSeq implements the RFC 3711 appendix A sequence extension.
func (t *Track) extendSeq(seq uint16) uint64 {
	if !t.started {
		t.started = true
		t.maxSeq = seq
		return uint64(seq)
	}
	roc := t.seqROC
	if seq < t.maxSeq {
		if t.maxSeq-seq > 30000 { // wrapped forward
			roc += 1 << 16
			t.seqROC = roc
			t.maxSeq = seq
		}
	} else if seq-t.maxSeq > 30000 {
		// A very old packet from before the last wrap.
		if roc == 0 {
			return uint64(seq)
		}
		return roc + uint64(seq) - (1 << 16)
	} else {
		t.maxSeq = seq
	}
	return roc + uint64(seq)
}

// extendTS extends the 32-bit RTP timestamp using its own wrap counter.
func (t *Track) extendTS(ts uint32) int64 {
	if ts < t.maxTS && t.maxTS-ts > 1<<31 {
		t.tsROC += 1 << 32
	}
	if ts > t.maxTS || t.tsROC == 0 {
		t.maxTS = ts
	}
	return t.tsROC + int64(ts)
}

// Registry routes raw datagrams to per-track state.
type Registry struct {
	tracks map[uint32]*Track
	order  []uint32 // sorted SSRCs for deterministic iteration
}

func NewRegistry() *Registry {
	return &Registry{tracks: make(map[uint32]*Track)}
}

// Register adds or fully replaces a track description.
func (r *Registry) Register(p TrackParams) {
	if _, ok := r.tracks[p.SSRC]; !ok {
		r.order = append(r.order, p.SSRC)
		for i := len(r.order) - 1; i > 0 && r.order[i] < r.order[i-1]; i-- {
			r.order[i], r.order[i-1] = r.order[i-1], r.order[i]
		}
	}
	t := r.tracks[p.SSRC]
	if t == nil {
		t = &Track{}
		r.tracks[p.SSRC] = t
	}
	t.Params = p
}

// UpdateParams replaces the signaled parameters of a known track,
// e.g. after a renegotiation. Reconstruction state is kept.
func (r *Registry) UpdateParams(p TrackParams) error {
	t, ok := r.tracks[p.SSRC]
	if !ok {
		return &media.Fault{Kind: media.FaultParse, Op: "ingest.track",
			Track: p.SSRC, At: -1, Msg: "update for unknown SSRC"}
	}
	t.Params = p
	return nil
}

// Tracks returns the registered SSRCs in sorted order.
func (r *Registry) Tracks() []uint32 { return append([]uint32(nil), r.order...) }

// Track returns the state for an SSRC, or nil.
func (r *Registry) Track(ssrc uint32) *Track { return r.tracks[ssrc] }

// Params returns the current signaling parameters of a track.
func (r *Registry) Params(ssrc uint32) (TrackParams, bool) {
	t, ok := r.tracks[ssrc]
	if !ok {
		return TrackParams{}, false
	}
	return t.Params, true
}

// Ingest parses one raw RTP datagram.
func (r *Registry) Ingest(raw []byte, at int64) (*Packet, *media.Fault) {
	var p rtp.Packet
	if err := p.Unmarshal(raw); err != nil {
		return nil, &media.Fault{Kind: media.FaultParse, Op: "ingest.rtp",
			At: at, Msg: err.Error()}
	}
	t := r.tracks[p.SSRC]
	if t == nil {
		return nil, &media.Fault{Kind: media.FaultParse, Op: "ingest.rtp",
			Track: p.SSRC, At: at, Msg: fmt.Sprintf("unknown SSRC %#x", p.SSRC)}
	}
	return &Packet{
		Track:   p.SSRC,
		Seq:     p.SequenceNumber,
		ExtSeq:  t.extendSeq(p.SequenceNumber),
		TS:      p.Timestamp,
		ExtTS:   t.extendTS(p.Timestamp),
		PT:      p.PayloadType,
		Marker:  p.Marker,
		Payload: p.Payload,
		At:      at,
	}, nil
}
