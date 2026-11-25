package transport

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/pion/rtp"

	"archivectl/internal/timeline"
)

// CaptureMagic identifies the deterministic length-framed RTP capture used by
// the local fixtures. Each frame is a big-endian uint32 length followed by one
// complete RTP datagram.
var CaptureMagic = []byte("ARTP0001")

const (
	MaxCaptureFrame = 64 * 1024
	ChunkHeaderSize = 8
	ChunkFlagStart  = 0x01
	ChunkFlagEnd    = 0x02
)

// ParseRTP validates and parses one RTP datagram using pion/rtp.
func ParseRTP(b []byte) (*rtp.Packet, error) {
	if len(b) == 0 {
		return nil, timeline.New(timeline.CodeInvalidArgument, "rtp parse", "empty packet")
	}
	var p rtp.Packet
	if err := p.Unmarshal(b); err != nil {
		return nil, timeline.Wrap(timeline.CodeInvalidArgument, "rtp parse", err)
	}
	return &p, nil
}

// WriteCapture writes length-framed RTP datagrams deterministically.
func WriteCapture(w io.Writer, packets [][]byte) error {
	if _, err := w.Write(CaptureMagic); err != nil {
		return err
	}
	for _, p := range packets {
		if len(p) == 0 || len(p) > MaxCaptureFrame {
			return timeline.New(timeline.CodeInvalidArgument, "write capture",
				fmt.Sprintf("packet length %d outside 1..%d", len(p), MaxCaptureFrame))
		}
		var ln [4]byte
		binary.BigEndian.PutUint32(ln[:], uint32(len(p)))
		if _, err := w.Write(ln[:]); err != nil {
			return err
		}
		if _, err := w.Write(p); err != nil {
			return err
		}
	}
	return nil
}

// ReadCapture reads the bounded length-framed capture format.
func ReadCapture(r io.Reader) ([][]byte, error) {
	magic := make([]byte, len(CaptureMagic))
	if _, err := io.ReadFull(r, magic); err != nil {
		return nil, timeline.Wrap(timeline.CodeInvalidArgument, "read capture magic", err)
	}
	if !bytes.Equal(magic, CaptureMagic) {
		return nil, timeline.New(timeline.CodeInvalidArgument, "read capture", "bad capture magic")
	}
	out := make([][]byte, 0, 128)
	for {
		var ln [4]byte
		_, err := io.ReadFull(r, ln[:])
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, timeline.Wrap(timeline.CodeCorruptBox, "read capture length", err)
		}
		n := binary.BigEndian.Uint32(ln[:])
		if n == 0 || n > MaxCaptureFrame {
			return nil, timeline.New(timeline.CodeInvalidArgument, "read capture",
				fmt.Sprintf("frame length %d outside 1..%d", n, MaxCaptureFrame))
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, timeline.Wrap(timeline.CodeCorruptBox, "read capture packet", err)
		}
		if _, err := ParseRTP(buf); err != nil {
			return nil, err
		}
		out = append(out, buf)
	}
	return out, nil
}

// ChunkHeader is the small fragmentation header carried in an RTP payload.
// RTP sequence numbers order chunks; SegmentID isolates fMP4 init and media
// segments, and Index makes missing chunks explicit.
type ChunkHeader struct {
	Flags     byte
	Index     uint16
	SegmentID uint32
}

func (h ChunkHeader) Marshal() []byte {
	b := make([]byte, ChunkHeaderSize)
	b[0] = h.Flags
	b[1] = 0
	binary.BigEndian.PutUint16(b[2:4], h.Index)
	binary.BigEndian.PutUint32(b[4:8], h.SegmentID)
	return b
}

func ParseChunkHeader(payload []byte) (ChunkHeader, []byte, error) {
	if len(payload) < ChunkHeaderSize {
		return ChunkHeader{}, nil, timeline.New(timeline.CodeInvalidArgument, "chunk header", "payload too short")
	}
	h := ChunkHeader{
		Flags:     payload[0],
		Index:     binary.BigEndian.Uint16(payload[2:4]),
		SegmentID: binary.BigEndian.Uint32(payload[4:8]),
	}
	return h, payload[ChunkHeaderSize:], nil
}

// BuildRTPPacket is a deterministic test/fixture helper.
func BuildRTPPacket(seq uint16, ts uint32, ssrc uint32, marker bool, payload []byte) ([]byte, error) {
	p := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           ssrc,
			Marker:         marker,
		},
		Payload: payload,
	}
	return p.Marshal()
}
