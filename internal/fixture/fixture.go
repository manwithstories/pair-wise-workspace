// Package fixture generates the local-only sample material used by the demo
// and tests: FSEQ frame sequences, fragmented MP4 and length-prefixed RTP logs
// with capture metadata, SHA-256 hashes and configurable impairments.
package fixture

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"math/rand"

	"github.com/pion/rtp"

	"relay/internal/demux"
	"relay/internal/media"
	"relay/internal/recovery"
)

func sha256Sum(p []byte) [32]byte { return sha256.Sum256(p) }

// Spec configures one generated capture source.
type Spec struct {
	SourceID    uint32
	Kind        media.Kind
	TimescaleHz uint32
	ClockRateHz uint32
	Width       uint32
	Height      uint32
	Samples     uint32
	DriftPPM    float64 // clock drift in parts per million
	StartRefNS  int64
	PayloadSeed int64
}

// Frames deterministically builds clean capture frames for one source. Each
// payload is opaque synthetic content; capture anchors include the requested
// constant drift so the clock stage has something observable to fit.
func Frames(s Spec) []*media.RawFrame {
	rng := rand.New(rand.NewSource(s.PayloadSeed))
	out := make([]*media.RawFrame, s.Samples)
	periodNS := int64(1_000_000_000 / int64(s.TimescaleHz))
	for i := uint32(0); i < s.Samples; i++ {
		payload := make([]byte, 96+(i%17))
		header := payload[:16]
		binary.BigEndian.PutUint32(header[0:4], s.SourceID)
		binary.BigEndian.PutUint32(header[4:8], i)
		binary.BigEndian.PutUint32(header[8:12], uint32(s.PayloadSeed))
		binary.BigEndian.PutUint32(header[12:16], uint32(len(payload)))
		if _, err := rng.Read(payload[16:]); err != nil {
			panic(err)
		}
		ideal := int64(i) * periodNS
		driftNS := int64(float64(ideal) * s.DriftPPM / 1e6)
		ref := s.StartRefNS + ideal + driftNS
		ticks := uint64(uint64(i) * uint64(s.ClockRateHz) / uint64(s.TimescaleHz))
		sum := sha256Sum(payload)
		out[i] = &media.RawFrame{
			Kind:    s.Kind,
			Payload: payload,
			Meta: media.FrameMeta{
				SourceID:       s.SourceID,
				Sequence:       uint64(i),
				ClockTicks:     ticks,
				CaptureRefNS:   ref,
				TimescaleHz:    uint64(s.ClockRateHz),
				PresentationNS: ideal,
				Width:          s.Width,
				Height:         s.Height,
				Size:           uint32(len(payload)),
				SHA256:         sum,
			},
		}
	}
	return out
}

// FSEQFile renders frames to an in-memory FSEQ1 file.
func FSEQFile(s Spec, frames []*media.RawFrame) []byte {
	var buf bytes.Buffer
	hdr := demux.FSEQHeader{
		SourceID: s.SourceID, Kind: s.Kind, TimescaleHz: s.TimescaleHz,
		Width: s.Width, Height: s.Height,
	}
	if err := demux.WriteFSEQ(&buf, hdr, frames); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// MP4File renders frames to an in-memory fragmented MP4 file.
func MP4File(s Spec, frames []*media.RawFrame) []byte {
	hdr := demux.FSEQHeader{
		SourceID: s.SourceID, Kind: s.Kind, TimescaleHz: s.TimescaleHz,
		Width: s.Width, Height: s.Height,
	}
	out, err := demux.WriteFragmentedMP4Bytes(hdr, frames)
	if err != nil {
		panic(err)
	}
	return out
}

// RTPImpairments selects which transport faults the RTP fixture contains.
type RTPImpairments struct {
	DuplicateAt  []uint16 // sequence numbers duplicated on the wire
	Reorder      bool     // swap two adjacent packets
	SingleLoss   []uint16 // one missing member per listed FEC group index
	DoubleLossAt []uint16 // two consecutive missing packets (unrecoverable)
}

// RTPLog builds a length-prefixed RTP capture log from spec, including XOR
// repair packets and the requested impairments. Group size is fixed at 8.
func RTPLog(s Spec, imp RTPImpairments) []byte {
	const groupSize = 8
	packets := buildMediaPackets(s)
	mediaBySeq := map[uint16]*rtp.Packet{}
	for _, p := range packets {
		mediaBySeq[p.SequenceNumber] = p
	}
	drop := map[uint16]bool{}
	for _, seq := range imp.SingleLoss {
		drop[seq] = true
	}
	for _, seq := range imp.DoubleLossAt {
		drop[seq] = true
		drop[seq+1] = true
	}
	dup := map[uint16]bool{}
	for _, seq := range imp.DuplicateAt {
		dup[seq] = true
	}
	wire := []*rtp.Packet{}
	groups := uint16((s.Samples + groupSize - 1) / groupSize)
	for g := uint16(0); g < groups; g++ {
		base := g * groupSize
		members := [][]byte{}
		for j := uint16(0); j < groupSize; j++ {
			seq := base + j
			if uint32(seq) >= s.Samples {
				break
			}
			members = append(members, mediaBySeq[seq].Payload)
			if drop[seq] {
				continue
			}
			wire = append(wire, mediaBySeq[seq])
			if dup[seq] {
				wire = append(wire, mediaBySeq[seq])
			}
		}
		if repair := buildRepair(s, mediaBySeq, base, members, drop); repair != nil {
			wire = append(wire, repair)
		}
	}
	if imp.Reorder && len(wire) > 3 {
		wire[0], wire[2] = wire[2], wire[0]
	}
	hdr := demux.RTPLogHeader{
		SourceID: s.SourceID, Kind: s.Kind, ClockRateHz: s.ClockRateHz,
		Width: s.Width, Height: s.Height,
	}
	var buf bytes.Buffer
	if err := demux.WriteRTPLog(&buf, hdr, wire); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func buildMediaPackets(s Spec) []*rtp.Packet {
	frames := Frames(s)
	out := make([]*rtp.Packet, len(frames))
	for i, fr := range frames {
		var anchor [8]byte
		binary.BigEndian.PutUint64(anchor[:], uint64(fr.Meta.CaptureRefNS))
		hdr := rtp.Header{
			Version: 2, Marker: true, PayloadType: 96,
			SequenceNumber: uint16(i), Timestamp: uint32(fr.Meta.ClockTicks),
			SSRC: s.SourceID + 0x1000,
		}
		out[i] = parseRaw(buildRawPacket(hdr, anchor[:], fr.Payload))
	}
	return out
}

// buildRawPacket assembles an RTP datagram with a raw extension area (profile
// BEDE, 3 words: one-byte element header + 8-byte anchor + padding). Building
// bytes directly avoids pion's one-byte sub-framing serializer.
func buildRawPacket(h rtp.Header, anchor, payload []byte) []byte {
	raw := make([]byte, 12+16+len(payload))
	raw[0] = 0x80 | 0x10 // V=2, X=1
	raw[1] = h.PayloadType
	if h.Marker {
		raw[1] |= 0x80
	}
	binary.BigEndian.PutUint16(raw[2:4], h.SequenceNumber)
	binary.BigEndian.PutUint32(raw[4:8], h.Timestamp)
	binary.BigEndian.PutUint32(raw[8:12], h.SSRC)
	binary.BigEndian.PutUint16(raw[12:14], recovery.CaptureRefExtProfile)
	binary.BigEndian.PutUint16(raw[14:16], 3) // 3 words = 12 data bytes
	raw[16] = 0x18                            // id=1, len-1=7 -> 8 bytes
	copy(raw[17:25], anchor)
	// raw[25:28] remain zero as extension padding.
	copy(raw[28:], payload)
	return raw
}

func parseRaw(raw []byte) *rtp.Packet {
	p := &rtp.Packet{}
	if err := p.Unmarshal(raw); err != nil {
		panic(err)
	}
	return p
}

// injectAnchor builds a repair packet with the same raw extension framing.
func injectAnchor(p *rtp.Packet, anchor []byte) *rtp.Packet {
	return parseRaw(buildRawPacket(p.Header, anchor, p.Payload))
}

func buildRepair(s Spec, mediaBySeq map[uint16]*rtp.Packet, base uint16, members [][]byte, drop map[uint16]bool) *rtp.Packet {
	if len(members) == 0 {
		return nil
	}
	payload := recovery.EncodeRepair(base, members)
	repair := &rtp.Packet{
		Header: rtp.Header{
			Version: 2, Marker: false, PayloadType: recovery.RepairPayloadType,
			SequenceNumber: base, Timestamp: mediaBySeq[base].Timestamp,
			SSRC:      s.SourceID + 0x1000,
			Extension: true, ExtensionProfile: recovery.CaptureRefExtProfile,
		},
		Payload: payload,
	}
	// Anchor on the first dropped member (single-loss case); the double-loss
	// anchor is irrelevant because that group never reconstructs.
	anchorSeq := base
	for j := 0; j < len(members); j++ {
		if drop[base+uint16(j)] {
			anchorSeq = base + uint16(j)
			break
		}
	}
	var anchor [8]byte
	binary.BigEndian.PutUint64(anchor[:], uint64(anchorFor(s, anchorSeq)))
	return injectAnchor(repair, anchor[:])
}

func anchorFor(s Spec, seq uint16) int64 {
	periodNS := int64(1_000_000_000 / int64(s.TimescaleHz))
	ideal := int64(seq) * periodNS
	return s.StartRefNS + ideal + int64(float64(ideal)*s.DriftPPM/1e6)
}
