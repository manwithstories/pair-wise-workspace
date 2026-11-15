// Package fixture creates deterministic H.264 Annex B access units and RTP packets.
package fixture

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pion/rtp"
)

const (
	ssrc         = 0x51a7c0de
	payloadType  = 96
	startCodeLen = 4
	fuChunkSize  = 1000
)

// Frame is one deterministic access unit. NALUs share the same RTP timestamp.
type Frame struct {
	Index     int
	Timestamp uint32
	Keyframe  bool
	NALUs     [][]byte
}

// Fixture is a generated stream. Parameters are repeated in keyframe AUs.
type Fixture struct {
	SPS    []byte
	PPS    []byte
	Frames []Frame
}

// Packet is an RTP packet plus the access unit it belongs to.
type Packet struct {
	RTP        rtp.Packet
	FrameIndex int
}

// Generate builds a stable fixture with one keyframe every 40 frames.
func Generate(frameCount int) (*Fixture, error) {
	if frameCount <= 0 {
		return nil, fmt.Errorf("frames must be positive")
	}
	f := &Fixture{
		SPS:    []byte{0x67, 0x42, 0x00, 0x1e, 0x95, 0xa8, 0x14, 0x01, 0x6e, 0x9b, 0x80},
		PPS:    []byte{0x68, 0xce, 0x38, 0x80},
		Frames: make([]Frame, frameCount),
	}
	for i := range f.Frames {
		key := i%40 == 0
		firstSize, secondSize := 56+(i*17)%160, 44+(i*29)%140
		if i%5 == 3 {
			firstSize = 2400
		}
		firstType, secondType := byte(0x41), byte(0x41)
		if key {
			firstType, secondType = 0x65, 0x65
			firstSize, secondSize = 96, 80
		}
		nals := make([][]byte, 0, 4)
		if key {
			nals = append(nals, clone(f.SPS), clone(f.PPS))
		}
		nals = append(nals,
			makeNAL(firstType, i, 0, firstSize),
			makeNAL(secondType, i, 1, secondSize),
		)
		f.Frames[i] = Frame{
			Index:     i,
			Timestamp: 90000 + uint32(i)*3000,
			Keyframe:  key,
			NALUs:     nals,
		}
	}
	return f, nil
}

func makeNAL(header byte, frame, slice, size int) []byte {
	nal := make([]byte, size)
	nal[0] = header
	for i := 1; i < len(nal); i++ {
		nal[i] = byte(16 + (frame*131+slice*71+i*13)%223)
	}
	return nal
}

func clone(b []byte) []byte { return append([]byte(nil), b...) }

// AnnexB serializes one access unit with four-byte start codes.
func (f Frame) AnnexB() []byte {
	var out []byte
	for _, nal := range f.NALUs {
		out = append(out, 0, 0, 0, 1)
		out = append(out, nal...)
	}
	return out
}

// WriteAnnexB writes all generated access units to path.
func (f *Fixture) WriteAnnexB(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var out []byte
	for _, frame := range f.Frames {
		out = append(out, frame.AnnexB()...)
	}
	return os.WriteFile(path, out, 0o644)
}

// Packetize turns access units into a deterministic RFC 6184 packet sequence.
func (f *Fixture) Packetize(firstSeq uint16) []Packet {
	seq := firstSeq
	out := make([]Packet, 0, len(f.Frames)*2)
	for _, frame := range f.Frames {
		payloads := payloadsForFrame(frame)
		for i, payload := range payloads {
			out = append(out, Packet{
				RTP: rtp.Packet{Header: rtp.Header{
					Version:        2,
					PayloadType:    payloadType,
					SequenceNumber: seq,
					Timestamp:      frame.Timestamp,
					SSRC:           ssrc,
					Marker:         i == len(payloads)-1,
				}, Payload: payload},
				FrameIndex: frame.Index,
			})
			seq++
		}
	}
	return out
}

func payloadsForFrame(frame Frame) [][]byte {
	nals := frame.NALUs
	switch {
	case frame.Keyframe:
		out := [][]byte{stapA(nals[0], nals[1])}
		for _, nal := range nals[2:] {
			out = append(out, singleNAL(nal))
		}
		return out
	case frame.Index%5 == 1:
		return [][]byte{stapA(nals...)}
	case frame.Index%5 == 3:
		out := fuA(nals[0])
		for _, nal := range nals[1:] {
			out = append(out, singleNAL(nal))
		}
		return out
	default:
		out := make([][]byte, 0, len(nals))
		for _, nal := range nals {
			out = append(out, singleNAL(nal))
		}
		return out
	}
}

func singleNAL(nal []byte) []byte { return clone(nal) }

func stapA(nals ...[]byte) []byte {
	out := []byte{0x18}
	for _, nal := range nals {
		var size [2]byte
		binary.BigEndian.PutUint16(size[:], uint16(len(nal)))
		out = append(out, size[:]...)
		out = append(out, nal...)
	}
	return out
}

func fuA(nal []byte) [][]byte {
	indicator := (nal[0] & 0xe0) | 28
	nalType := nal[0] & 0x1f
	body := nal[1:]
	out := make([][]byte, 0, (len(body)+fuChunkSize-1)/fuChunkSize)
	for off := 0; off < len(body); off += fuChunkSize {
		end := off + fuChunkSize
		if end > len(body) {
			end = len(body)
		}
		fuHeader := nalType
		if off == 0 {
			fuHeader |= 0x80
		}
		if end == len(body) {
			fuHeader |= 0x40
		}
		payload := []byte{indicator, fuHeader}
		payload = append(payload, body[off:end]...)
		out = append(out, payload)
	}
	return out
}

// FirstSequence is the stable starting sequence used by the CLI.
func FirstSequence(seed int64) uint16 { return uint16(4080 + (seed%16+16)%16) }
