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
	var buf bytes.Buffer
	seeker := newWriteSeeker(&buf)
	hdr := demux.FSEQHeader{
		SourceID: s.SourceID, Kind: s.Kind, TimescaleHz: s.TimescaleHz,
		Width: s.Width, Height: s.Height,
	}
	if err := demux.WriteFragmentedMP4(seeker, hdr, frames); err != nil {
		panic(err)
	}
	return buf.Bytes()
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
		p := &rtp.Packet{
			Header: rtp.Header{
				Version: 2, Marker: true, PayloadType: 96,
				SequenceNumber: uint16(i), Timestamp: uint32(fr.Meta.ClockTicks),
				SSRC:      s.SourceID + 0x1000,
				Extension: true, ExtensionProfile: recovery.CaptureRefExtProfile,
			},
			Payload: append([]byte(nil), fr.Payload...),
		}
		out[i] = injectAnchor(p, anchor[:])
	}
	return out
}

// injectAnchor appends an RFC 8285 one-byte extension element (id 1, 8-byte
// anchor) to a packet by editing its marshalled bytes, then re-parsing it.
func injectAnchor(p *rtp.Packet, anchor []byte) *rtp.Packet {
	base, err := p.Marshal()
	if err != nil {
		panic(err)
	}
	// Strip any auto-emitted extension area and rebuild one of exactly 12 bytes.
	csrc := int(base[0] & 0x0f)
	headerLen := 12 + 4*csrc
	fixed := make([]byte, headerLen)
	copy(fixed, base[:headerLen])
	fixed[0] |= 0x10 // X bit
	ext := make([]byte, 12)
	binary.BigEndian.PutUint16(ext[0:2], recovery.CaptureRefExtProfile)
	binary.BigEndian.PutUint16(ext[2:4], 2) // 2 words = 8 data bytes
	ext[4] = 0x18                           // id=1, len-1=7 -> 8 bytes
	copy(ext[5:13], anchor)
	raw := append(append(append([]byte(nil), fixed...), ext...), base[headerLen:]...)
	reparsed := &rtp.Packet{}
	if err := reparsed.Unmarshal(raw); err != nil {
		panic(err)
	}
	return reparsed
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

// writeSeeker adapts bytes.Buffer to io.WriteSeeker for the MP4 writer.
type writeSeeker struct {
	buf *bytes.Buffer
	pos int64
}

func newWriteSeeker(b *bytes.Buffer) *writeSeeker { return &writeSeeker{buf: b} }

func (w *writeSeeker) Write(p []byte) (int, error) {
	end := w.pos + int64(len(p))
	if end > int64(w.buf.Len()) {
		grow := make([]byte, end-int64(w.buf.Len()))
		w.buf.Write(grow)
	}
	copy(w.buf.Bytes()[w.pos:end], p)
	w.pos = end
	return len(p), nil
}

func (w *writeSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case 0:
		w.pos = offset
	case 1:
		w.pos += offset
	case 2:
		w.pos = int64(w.buf.Len()) + offset
	}
	if w.pos < 0 {
		return 0, bytes.ErrTooLarge
	}
	return w.pos, nil
}
