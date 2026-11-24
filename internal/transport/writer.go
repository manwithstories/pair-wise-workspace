package transport

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/pion/rtp"
)

// EncodeFrame produces one .rtpr frame: fixed 12-byte header, rep id and a
// 2-byte length-prefixed raw RTP packet. It is the writer-side counterpart of
// ReadRecord and is shared by the fixture generator and round-trip tests.
func EncodeFrame(repID string, fragment int, arrivalUS uint32, rawRTP []byte) ([]byte, error) {
	if len(repID) > 255 {
		return nil, errors.New("transport: rep id too long")
	}
	if fragment < 0 || fragment > 65535 {
		return nil, fmt.Errorf("transport: fragment %d out of frame range", fragment)
	}
	if len(rawRTP) > 65535 {
		return nil, errors.New("transport: RTP packet too large for frame")
	}
	out := make([]byte, 0, headerSize+len(repID)+2+len(rawRTP))
	out = append(out, rtprMagic...)
	out = append(out, rtprVersion)
	out = append(out, byte(len(repID)))
	out = binary.BigEndian.AppendUint16(out, uint16(fragment))
	out = binary.BigEndian.AppendUint32(out, arrivalUS)
	out = append(out, repID...)
	out = binary.BigEndian.AppendUint16(out, uint16(len(rawRTP)))
	out = append(out, rawRTP...)
	return out, nil
}

// NewPacket builds an RTP packet whose one-element extension maps it to a
// fragment and a payload byte offset, matching DecodeFragmentExtension.
func NewPacket(seq uint16, fragment int, offset uint32, payload []byte) (*rtp.Packet, error) {
	ext := make([]byte, 6)
	binary.BigEndian.PutUint16(ext[0:2], uint16(fragment))
	binary.BigEndian.PutUint32(ext[2:6], offset)
	p := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: seq,
			SSRC:           0x4d454449,
		},
		Payload: append([]byte(nil), payload...),
	}
	if err := p.SetExtension(ExtensionID, ext); err != nil {
		return nil, err
	}
	return p, nil
}
