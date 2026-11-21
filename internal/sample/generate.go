// Package sample generates deterministic MPEG-TS fixtures entirely in
// code: program tables, a video PES track with PTS/PCR and keyframes,
// and an RTP data track (pion/rtp) for packet-boundary/sequence
// samples. Fault injection covers PCR wrap, continuity-counter gaps,
// PSI version updates and end-of-stream truncation.
package sample

import (
	"github.com/pion/rtp"

	"tsbatch/internal/ts"
)

// Options controls fixture generation. Zero CCGapAt/PSIUpdateAt mean
// "disabled" (use -1 semantics via the Enabled flags below).
type Options struct {
	Packets          int  // total TS packets before truncation
	PCRWrap          bool // start PCR so it wraps exactly once mid-stream
	CCGapAt          int  // video packet ordinal carrying the CC jump, -1 = off
	CCGapSize        int  // extra counter steps injected at CCGapAt
	PSIUpdateAt      int  // packet index from which PMT version bumps, -1 = off
	TruncateBytes    int  // dangling bytes appended after the last packet
	KeyframeInterval int  // frames between keyframes (default 25)
}

type gen struct {
	cc       map[uint16]uint8
	out      []byte
	keyEvery int
}

// Generate builds the full stream deterministically.
func Generate(o Options) []byte {
	if o.KeyframeInterval <= 0 {
		o.KeyframeInterval = 25
	}
	if o.CCGapSize <= 0 {
		o.CCGapSize = 2
	}
	g := &gen{cc: map[uint16]uint8{}, keyEvery: o.KeyframeInterval}

	// Two passes: count video packets to size the PCR wrap offset.
	videoPkts := 0
	for i := 0; i < o.Packets; i++ {
		if g.slotKind(i) == 'v' {
			videoPkts++
		}
	}
	frames := (videoPkts + 2) / 3
	duration := uint64(frames) * 3600 // 25 fps in 90 kHz units
	pcrBase := uint64(90000)
	if o.PCRWrap {
		pcrBase = (1 << 33) - duration/2 - 45000
	}

	frame := -1
	var pes []byte // remaining PES bytes of the open frame
	pusi := false
	vOrd := 0 // video packet ordinal
	rtpSeq := uint16(7)
	for i := 0; i < o.Packets; i++ {
		version := uint8(0)
		if o.PSIUpdateAt >= 0 && i >= o.PSIUpdateAt {
			version = 1
		}
		switch g.slotKind(i) {
		case 'p':
			g.emit(PSIPacket(PIDPAT, 0, g.nextCC(PIDPAT))) // PAT stays at v0
		case 'm':
			g.emit(PSIPacket(PIDPMT, version, g.nextCC(PIDPMT)))
		case 'd':
			rtpSeq++
			g.emit(g.dataPacket(rtpSeq))
		default: // video
			if len(pes) == 0 {
				frame++
				pes = buildPES(pcrBase, frame, frame%o.KeyframeInterval == 0)
				pusi = true
			}
			pcr := uint64(0)
			hasPCR := pusi
			if hasPCR {
				pcr = (pcrBase + uint64(frame)*3600) & ((1 << 33) - 1)
			}
			cap := 184
			if hasPCR {
				cap = 176
			}
			chunk := pes
			if len(chunk) > cap {
				chunk = pes[:cap]
			}
			pes = pes[len(chunk):]
			ccGap := 0
			if vOrd == o.CCGapAt {
				ccGap = o.CCGapSize
			}
			g.emit(g.videoPacket(chunk, pusi, hasPCR, pcr,
				frame%o.KeyframeInterval == 0 && pusi, ccGap))
			pusi = false
			vOrd++
		}
	}
	if o.TruncateBytes > 0 {
		g.out = append(g.out, make([]byte, o.TruncateBytes)...)
	}
	return g.out
}

// slotKind classifies a packet index: 'p' PAT, 'm' PMT, 'd' data, 'v' video.
func (g *gen) slotKind(i int) byte {
	if i%40 == 0 {
		return 'p'
	}
	if i%40 == 1 {
		return 'm'
	}
	if i%15 == 2 {
		return 'd'
	}
	return 'v'
}

func (g *gen) emit(pkt [ts.PacketSize]byte) { g.out = append(g.out, pkt[:]...) }

func (g *gen) nextCC(pid uint16) uint8 {
	cc := g.cc[pid]
	g.cc[pid] = (cc + 1) & 0x0F
	return cc
}

// videoPacket builds one video packet; ccGap injects extra counter steps.
func (g *gen) videoPacket(payload []byte, pusi, hasPCR bool, pcr uint64, randomAccess bool, ccGap int) [ts.PacketSize]byte {
	g.cc[PIDVideo] = (g.cc[PIDVideo] + uint8(ccGap)) & 0x0F
	cc := g.nextCC(PIDVideo)
	var p [ts.PacketSize]byte
	p[0] = ts.SyncByte
	p[1] = byte(PIDVideo >> 8)
	if pusi {
		p[1] |= 0x40
	}
	p[2] = byte(PIDVideo & 0xFF)
	if len(payload) == 184 && !hasPCR && !randomAccess {
		p[3] = 0x10 | cc // payload only, no adaptation field
		copy(p[4:], payload)
		return p
	}
	// Otherwise adaptation+payload: stuffing lands in the adaptation
	// field; PCR rides on the first packet of each frame.
	afl := 183 - len(payload)
	p[3] = 0x30 | cc
	p[4] = byte(afl)
	flags := byte(0)
	off := 5
	if afl >= 1 {
		if randomAccess {
			flags |= 0x40
		}
		if hasPCR {
			flags |= 0x10
		}
		p[5] = flags
		off = 6
		if hasPCR {
			p[6] = byte(pcr >> 25)
			p[7] = byte(pcr >> 17)
			p[8] = byte(pcr >> 9)
			p[9] = byte(pcr >> 1)
			p[10] = byte(pcr<<7) | 0x7E
			p[11] = 0
			off = 12
		}
		for j := off; j < 5+afl; j++ {
			p[j] = 0xFF
		}
	}
	copy(p[5+afl:], payload)
	return p
}

// dataPacket emits one PES-wrapped RTP packet on the data PID.
func (g *gen) dataPacket(seq uint16) [ts.PacketSize]byte {
	rtpPkt := &rtp.Packet{
		Header: rtp.Header{
			Version: 2, PayloadType: 96, SequenceNumber: seq,
			Timestamp: uint32(seq) * 3000, SSRC: 0x12781,
		},
		Payload: []byte{0xDE, 0xAD, byte(seq), byte(seq >> 8)},
	}
	rtpRaw, _ := rtpPkt.Marshal()
	pes := []byte{0x00, 0x00, 0x01, 0xBD, byte(len(rtpRaw) >> 8), byte(len(rtpRaw)), 0x80, 0x00, 0x00}
	pes = append(pes, rtpRaw...)
	var p [ts.PacketSize]byte
	p[0] = ts.SyncByte
	p[1] = 0x40 | byte(PIDData>>8)
	p[2] = byte(PIDData & 0xFF)
	afl := 183 - len(pes)
	p[3] = 0x30 | g.nextCC(PIDData)
	p[4] = byte(afl)
	if afl >= 1 {
		p[5] = 0
		for j := 6; j < 5+afl; j++ {
			p[j] = 0xFF
		}
	}
	copy(p[5+afl:], pes)
	return p
}
