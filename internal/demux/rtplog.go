package demux

import (
	"encoding/binary"
	"errors"
	"io"

	"github.com/pion/rtp"

	"relay/internal/media"
)

// RTPLogHeader leads a length-prefixed RTP capture log. It carries the capture
// context that RTP itself cannot express: source identity, media kind, clock
// rate and optional frame dimensions.
type RTPLogHeader struct {
	Magic       [4]byte // 'R','T','P','L'
	Version     uint8
	SourceID    uint32
	Kind        media.Kind
	ClockRateHz uint32
	Width       uint32
	Height      uint32
	PacketCount uint32
}

// rtpLogHeaderSize is the fixed 26-byte on-disk header.
const rtpLogHeaderSize = 26

// RTPLRecord is one length-prefixed RTP packet record.
type RTPLRecord struct {
	Length uint16
	Packet rtp.Packet
}

var rtpLogMagic = [4]byte{'R', 'T', 'P', 'L'}

// WriteRTPLog writes a capture log from raw RTP packets. Each record is a
// big-endian u16 length followed by the marshalled packet bytes.
func WriteRTPLog(w io.Writer, hdr RTPLogHeader, packets []*rtp.Packet) error {
	hdr.Magic = rtpLogMagic
	hdr.Version = 1
	hdr.PacketCount = uint32(len(packets))
	if hdr.PacketCount > media.MaxTotalSamples {
		return media.NewError(media.ErrLimitExceeded, hdr.SourceID, "packet count over limit")
	}
	buf := make([]byte, rtpLogHeaderSize)
	copy(buf[0:4], hdr.Magic[:])
	buf[4] = hdr.Version
	bin := binary.BigEndian
	bin.PutUint32(buf[5:9], hdr.SourceID)
	buf[9] = byte(hdr.Kind)
	bin.PutUint32(buf[10:14], hdr.ClockRateHz)
	bin.PutUint32(buf[14:18], hdr.Width)
	bin.PutUint32(buf[18:22], hdr.Height)
	bin.PutUint32(buf[22:26], hdr.PacketCount)
	if _, err := w.Write(buf[:26]); err != nil {
		return err
	}
	for _, pkt := range packets {
		raw, err := pkt.Marshal()
		if err != nil {
			return media.NewError(media.ErrMetadata, hdr.SourceID, "rtp marshal: "+err.Error())
		}
		if len(raw) > media.MaxPacketBytes {
			return media.NewError(media.ErrLimitExceeded, hdr.SourceID, "packet too large")
		}
		var lenBuf [2]byte
		bin.PutUint16(lenBuf[:], uint16(len(raw)))
		if _, err := w.Write(lenBuf[:]); err != nil {
			return err
		}
		if _, err := w.Write(raw); err != nil {
			return err
		}
	}
	return nil
}

// RTPPacket is a parsed record with its raw wire bytes retained so the
// recovery stage can run XOR FEC over exact payloads.
type RTPPacket struct {
	Raw    []byte
	Packet *rtp.Packet
}

// ReadRTPLog parses a capture log, validating the header, length prefixes and
// RTP framing. Reordering, duplicates and gaps are resolved later by recovery.
func ReadRTPLog(r io.Reader) ([]RTPPacket, RTPLogHeader, error) {
	var hdrBuf [26]byte
	if _, err := io.ReadFull(r, hdrBuf[:]); err != nil {
		return nil, RTPLogHeader{}, media.NewError(media.ErrTruncated, 0, "missing RTPL header: "+err.Error())
	}
	hdr := RTPLogHeader{}
	copy(hdr.Magic[:], hdrBuf[0:4])
	if hdr.Magic != rtpLogMagic {
		return nil, hdr, media.NewError(media.ErrMetadata, 0, "bad RTPL magic")
	}
	hdr.Version = hdrBuf[4]
	if hdr.Version != 1 {
		return nil, hdr, media.NewError(media.ErrMetadata, 0, "unsupported RTPL version")
	}
	bin := binary.BigEndian
	hdr.SourceID = bin.Uint32(hdrBuf[5:9])
	hdr.Kind = media.Kind(hdrBuf[9])
	hdr.ClockRateHz = bin.Uint32(hdrBuf[10:14])
	hdr.Width = bin.Uint32(hdrBuf[14:18])
	hdr.Height = bin.Uint32(hdrBuf[18:22])
	hdr.PacketCount = bin.Uint32(hdrBuf[22:26])
	if hdr.Kind != media.KindVideo && hdr.Kind != media.KindAudio {
		return nil, hdr, media.NewError(media.ErrMetadata, hdr.SourceID, "unknown media kind")
	}
	if uint64(hdr.PacketCount) > media.MaxTotalSamples {
		return nil, hdr, media.NewError(media.ErrLimitExceeded, hdr.SourceID, "packet count over limit")
	}
	out := make([]RTPPacket, 0, hdr.PacketCount)
	var lenBuf [2]byte
	for i := uint32(0); i < hdr.PacketCount; i++ {
		if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
			return nil, hdr, media.NewError(media.ErrTruncated, hdr.SourceID, "length prefix")
		}
		length := bin.Uint16(lenBuf[:])
		if length == 0 || uint32(length) > media.MaxPacketBytes {
			return nil, hdr, media.NewError(media.ErrLengthMismatch, hdr.SourceID, "record length out of range")
		}
		raw := make([]byte, length)
		if _, err := io.ReadFull(r, raw); err != nil {
			return nil, hdr, media.NewError(media.ErrTruncated, hdr.SourceID, "rtp packet")
		}
		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(raw); err != nil {
			return nil, hdr, media.NewError(media.ErrMetadata, hdr.SourceID, "rtp unmarshal: "+err.Error())
		}
		out = append(out, RTPPacket{Raw: raw, Packet: pkt})
	}
	// Reject trailing bytes like the FSEQ reader.
	if n, err := r.Read(make([]byte, 1)); err == nil && n > 0 {
		return nil, hdr, media.NewError(media.ErrLengthMismatch, hdr.SourceID, "unexpected trailing bytes")
	} else if err != nil && !errors.Is(err, io.EOF) {
		return nil, hdr, err
	}
	return out, hdr, nil
}
