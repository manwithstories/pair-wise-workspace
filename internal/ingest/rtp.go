// Package ingest reorders RTP preview packets by sequence number and reports
// loss, duplication and reordering. A missing packet becomes an explicit gap
// that later flows into the manifest; it is never papered over with a
// synthesized frame.
package ingest

import (
	"encoding/binary"
	"errors"
	"sort"

	"github.com/pion/rtp"

	"mediad/internal/model"
)

// PreviewPacket is one preview RTP packet carrying a fragment of a frame.
// FrameNo is decoded from the payload prefix the muxer prepends.
type PreviewPacket struct {
	Seq     uint16
	FrameNo uint64
	Marker  bool // last fragment of a frame
	Payload []byte
}

// EncodePreview builds an RTP packet whose payload begins with the frame
// number encoded as a 64-bit big-endian value.
func EncodePreview(p PreviewPacket, ssrc uint32) ([]byte, error) {
	pl := make([]byte, 8+len(p.Payload))
	binary.BigEndian.PutUint64(pl, p.FrameNo)
	copy(pl[8:], p.Payload)
	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: p.Seq,
			Timestamp:      uint32(p.FrameNo),
			SSRC:           ssrc,
			Marker:         p.Marker,
		},
		Payload: pl,
	}
	return pkt.Marshal()
}

// DecodePreview parses a marshalled RTP packet.
func DecodePreview(buf []byte) (PreviewPacket, error) {
	var pkt rtp.Packet
	if err := pkt.Unmarshal(buf); err != nil {
		return PreviewPacket{}, model.Recoverable("rtp.decode", 0, 0, "unmarshal", err)
	}
	if len(pkt.Payload) < 8 {
		return PreviewPacket{}, model.Recoverable("rtp.decode", 0, 0, "short_payload", errors.New("preview payload shorter than its frame prefix"))
	}
	return PreviewPacket{
		Seq:     pkt.SequenceNumber,
		FrameNo: binary.BigEndian.Uint64(pkt.Payload[:8]),
		Marker:  pkt.Marker,
		Payload: append([]byte(nil), pkt.Payload[8:]...),
	}, nil
}

// Gap is an inclusive range of missing sequence numbers and the frame whose
// fragment was expected there.
type Gap struct {
	FromSeq, ToSeq uint16
	InferredFrame  uint64
}

// RTPStats summarizes a reassembly pass.
type RTPStats struct {
	Received   int // unique packets
	Ordered    int // complete frames reassembled
	Duplicates int
	Reordered  int // packets that arrived out of sequence
	Lost       int // missing sequence numbers
	Gaps       []Gap
	Frames     []uint64 // frame numbers fully reassembled, ascending
}

// Reorder sorts packets by sequence number with 16-bit wrap handling, detects
// duplicates and holes, and collects frames whose fragments all arrived.
//
// Reordering is measured against arrival order, not the sorted order, so a
// deliberately shuffled preview reports it. A hole is loss with its size; the
// packets after it are still ordered and their frames still emitted, so one
// lost preview packet never discards the rest.
//
// Completeness is judged by the marker on the highest-sequence fragment of a
// frame, located after unwrapping, so fragments that straddle a 16-bit wrap
// are not mistaken for incomplete.
func Reorder(packets []PreviewPacket) (*RTPStats, error) {
	st := &RTPStats{}
	if len(packets) == 0 {
		return st, nil
	}
	// Arrival order: a packet is reordered if a later-arriving packet has a
	// smaller sequence number (modulo wrap) than one already seen.
	var maxKey int32
	base := packets[0].Seq
	for _, p := range packets[1:] {
		if int16(p.Seq-base) < 0 {
			base = p.Seq
		}
	}
	key := func(seq uint16) int32 { return int32(int16(seq - base)) }
	first := true
	for _, p := range packets {
		k := key(p.Seq)
		if !first && k < maxKey {
			st.Reordered++
		}
		if first || k > maxKey {
			maxKey = k
			first = false
		}
	}

	sorted := append([]PreviewPacket(nil), packets...)
	sort.SliceStable(sorted, func(i, j int) bool { return key(sorted[i].Seq) < key(sorted[j].Seq) })

	// Walk the sorted stream once, counting holes and folding duplicates.
	seen := make(map[uint16]bool, len(sorted))
	frags := make(map[uint64][]int32) // frame -> unwrapped sequence keys
	var prevKey int32
	havePrev := false
	for _, p := range sorted {
		k := key(p.Seq)
		if seen[p.Seq] {
			st.Duplicates++
			continue
		}
		seen[p.Seq] = true
		st.Received++
		if havePrev && k > prevKey+1 {
			loss := k - prevKey - 1
			st.Lost += int(loss)
			st.Gaps = append(st.Gaps, Gap{FromSeq: p.Seq - uint16(loss), ToSeq: p.Seq - 1, InferredFrame: p.FrameNo})
		}
		havePrev, prevKey = true, k
		frags[p.FrameNo] = append(frags[p.FrameNo], k)
	}

	// A frame is complete when its highest-sequence fragment carries a marker.
	for fn, keys := range frags {
		maxKey := keys[0]
		for _, k := range keys[1:] {
			if k > maxKey {
				maxKey = k
			}
		}
		if markerAt(sorted, fn, maxKey, key) {
			st.Frames = append(st.Frames, fn)
		}
	}
	sort.Slice(st.Frames, func(i, j int) bool { return st.Frames[i] < st.Frames[j] })
	st.Ordered = len(st.Frames)
	return st, nil
}

func markerAt(sorted []PreviewPacket, frame uint64, seqKey int32, key func(uint16) int32) bool {
	for _, p := range sorted {
		if p.FrameNo == frame && key(p.Seq) == seqKey {
			return p.Marker
		}
	}
	return false
}

// ErrNoPreview reports a preview stream with no packets.
var ErrNoPreview = errors.New("ingest: empty preview stream")

// MissingFrames returns the frame numbers absent between the first and last
// complete frame. These become explicit manifest gaps; no frame is invented.
func MissingFrames(st *RTPStats) []uint64 {
	if len(st.Frames) == 0 {
		return nil
	}
	have := make(map[uint64]bool, len(st.Frames))
	for _, f := range st.Frames {
		have[f] = true
	}
	var out []uint64
	for f := st.Frames[0]; f <= st.Frames[len(st.Frames)-1]; f++ {
		if !have[f] {
			out = append(out, f)
		}
	}
	return out
}

// BuildPreview generates the deterministic preview stream for a track: two
// fragments per frame, with optional packet loss, duplication and shuffling.
func BuildPreview(trackID uint32, frames int, seed int64, drop, dupEvery int, shuffle bool) ([]PreviewPacket, error) {
	if frames <= 0 {
		return nil, ErrNoPreview
	}
	var out []PreviewPacket
	var seq uint16 = 1000
	rng := newXorshift(seed)
	dropSet := make(map[int]bool)
	for i := 0; i < drop; i++ {
		// Spread drops away from the very first/last frames.
		dropSet[2+i*3%(frames*2-1)] = true
	}
	for f := 1; f <= frames; f++ {
		for frag := 0; frag < 2; frag++ {
			idx := (f-1)*2 + frag
			if dropSet[idx] {
				seq++ // the packet existed on the sender; it is simply lost
				continue
			}
			p := PreviewPacket{
				Seq: seq, FrameNo: uint64(f), Marker: frag == 1,
				Payload: []byte{byte(trackID), byte(f), byte(frag)},
			}
			out = append(out, p)
			seq++
			if dupEvery > 0 && idx%dupEvery == dupEvery-1 {
				out = append(out, p) // exact duplicate
			}
		}
	}
	if shuffle {
		for i := len(out) - 1; i > 0; i-- {
			j := int(rng.next() % uint64(i+1))
			out[i], out[j] = out[j], out[i]
		}
	}
	return out, nil
}

// xorshift is a tiny deterministic PRNG so fixtures never depend on the
// math/rand sequence of a Go release.
type xorshift struct{ state uint64 }

func newXorshift(seed int64) *xorshift {
	s := uint64(seed) | 1
	return &xorshift{state: s*6364136223846793005 + 1442695040888963407}
}

func (x *xorshift) next() uint64 {
	x.state ^= x.state << 13
	x.state ^= x.state >> 7
	x.state ^= x.state << 17
	return x.state
}
