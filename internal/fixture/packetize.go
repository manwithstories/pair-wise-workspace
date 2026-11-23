package fixture

import (
	"encoding/binary"
	"math/rand"

	"github.com/pion/rtp"
)

// RTPMTU is the maximum RTP payload size per generated packet.
const RTPMTU = 1200

// RTPClockRate is the 90 kHz video RTP clock used by generated streams.
const RTPClockRate uint32 = 90000

// Packet wraps a marshalled RTP packet with the sequence number the
// receiver expects to see.
type Packet struct {
	Seq  uint16
	Raw  []byte
	SSRC uint32
}

// BuildPackets renders the deterministic, fault-injected RTP packet
// sequence for the spec. StartSeq allows callers to force a 16-bit
// wraparound near the beginning of the stream.
func (s Spec) BuildPackets(startSeq uint16) ([]Packet, error) {
	var ordered []Packet
	ssrc := uint32(0x4d450000) ^ uint32(s.Seed)
	seq := uint32(startSeq)
	ts := uint32(0)
	tsStep := uint32(uint64(RTPClockRate) * uint64(s.FrameDelta) / uint64(s.Timescale))
	for _, sp := range s.AllSamples() {
		data := make([]byte, sp.Size)
		SampleData(s, sp.Index, data)
		first := true
		for off := 0; off < len(data); {
			n := RTPMTU
			if rem := len(data) - off; rem < n {
				n = rem
			}
			hdr := make([]byte, PayloadHeaderSize)
			binary.BigEndian.PutUint32(hdr[0:], uint32(sp.Index))
			binary.BigEndian.PutUint32(hdr[4:], uint32(off))
			binary.BigEndian.PutUint16(hdr[8:], uint16(sp.Size))
			flags := uint16(0)
			if first {
				flags |= FlagFirst
			}
			if off+n == len(data) {
				flags |= FlagLast
			}
			if sp.Keyframe {
				flags |= FlagKey
			}
			binary.BigEndian.PutUint16(hdr[12:], flags)
			pkt := &rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					PayloadType:    96,
					SequenceNumber: uint16(seq),
					Timestamp:      ts,
					SSRC:           ssrc,
					Marker:         off+n == len(data),
				},
				Payload: append(hdr, data[off:off+n]...),
			}
			raw, err := pkt.Marshal()
			if err != nil {
				return nil, err
			}
			ordered = append(ordered, Packet{
				Seq: uint16(seq), Raw: raw, SSRC: ssrc,
			})
			seq++
			first = false
			off += n
		}
		ts += tsStep
	}
	return s.injectFaults(ordered), nil
}

// injectFaults applies burst drops, duplicates and bounded reordering.
// All operations are deterministic for a given spec/seed.
func (s Spec) injectFaults(in []Packet) []Packet {
	drop := map[uint16]bool{}
	if s.DropBurstN > 0 && s.DropBurstAt >= 0 {
		for i := 0; i < s.DropBurstN; i++ {
			drop[uint16(s.DropBurstAt+i)] = true
		}
	}
	out := make([]Packet, 0, len(in)+s.DuplicateN)
	for _, p := range in {
		if drop[p.Seq] {
			continue
		}
		out = append(out, p)
	}
	if s.DuplicateN > 0 && len(out) > 2 {
		rnd := rand.New(rand.NewSource(s.Seed + 7))
		for i := 0; i < s.DuplicateN; i++ {
			idx := rnd.Intn(len(out))
			out = append(out, out[idx])
		}
	}
	if s.ReorderWin > 1 {
		rnd := rand.New(rand.NewSource(s.Seed + 13))
		win := s.ReorderWin
		for i := 0; i+win < len(out); i += win {
			slice := out[i : i+win]
			rnd.Shuffle(win, func(a, b int) {
				slice[a], slice[b] = slice[b], slice[a]
			})
		}
	}
	return out
}
