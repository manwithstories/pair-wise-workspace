// Package ingest is the entry boundary of the pipeline. It unpacks RTP
// packets (MPEG-TS over RTP per RFC 2250, each payload holding an integer
// number of 188-byte TS packets) and also accepts raw MPEG-TS byte streams.
package ingest

import (
	"encoding/binary"
	"io"

	"github.com/pion/rtp"

	"streamcut/internal/media"
	"streamcut/internal/ts"
)

// Framing selects how the input byte stream is interpreted.
type Framing int

const (
	FramingAuto Framing = iota // sniff the first byte
	FramingRTP                 // 2-byte BE length-prefixed RTP packets
	FramingRawTS              // concatenated 188-byte TS packets
)

// RTPReader decodes length-framed RTP packets and exposes their TS payloads.
type RTPReader struct {
	r       io.Reader
	lastSeq uint16
	haveSeq bool
	// Lost counts RTP sequence-number gaps, which indicate transport loss
	// upstream of the TS layer.
	Lost int
}

// NewRTPReader wraps a framed RTP byte stream.
func NewRTPReader(r io.Reader) *RTPReader { return &RTPReader{r: r} }

// ReadRTPPacket reads one length-prefixed RTP packet.
func (rd *RTPReader) ReadRTPPacket() (*rtp.Packet, error) {
	var lenBuf [2]byte
	if _, err := io.ReadFull(rd.r, lenBuf[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint16(lenBuf[:])
	if n == 0 {
		return nil, media.Wrap(media.EUnrecoverable, "ingest.rtp",
			"zero-length RTP frame", nil)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(rd.r, buf); err != nil {
		return nil, media.Wrap(media.ETruncated, "ingest.rtp",
			"truncated RTP frame", err)
	}
	pkt := &rtp.Packet{}
	if err := pkt.Unmarshal(buf); err != nil {
		return nil, media.Wrap(media.EUnrecoverable, "ingest.rtp",
			"malformed RTP packet", err)
	}
	if rd.haveSeq {
		if want := (rd.lastSeq + 1) & 0xFFFF; pkt.SequenceNumber != want {
			rd.Lost += int((pkt.SequenceNumber - rd.lastSeq) & 0xFFFF) - 1
		}
	}
	rd.haveSeq = true
	rd.lastSeq = pkt.SequenceNumber
	return pkt, nil
}

// ReadTSPackets returns the 188-byte TS packets carried by the next RTP
// packet. A payload not divisible by 188 is unrecoverable corruption.
func (rd *RTPReader) ReadTSPackets() ([][]byte, error) {
	pkt, err := rd.ReadRTPPacket()
	if err != nil {
		return nil, err
	}
	if len(pkt.Payload)%ts.PacketSize != 0 {
		return nil, media.Wrap(media.EUnrecoverable, "ingest.rtp",
			"RTP payload is not a whole number of TS packets", nil)
	}
	out := make([][]byte, 0, len(pkt.Payload)/ts.PacketSize)
	for off := 0; off < len(pkt.Payload); off += ts.PacketSize {
		out = append(out, pkt.Payload[off:off+ts.PacketSize])
	}
	return out, nil
}

// ReadRawTS feeds every 188-byte packet of a raw TS stream to fn. A final
// short read is reported as ErrTruncated (the damaged fixture relies on it).
func ReadRawTS(r io.Reader, fn func([]byte) error) error {
	buf := make([]byte, ts.PacketSize)
	for {
		_, err := io.ReadFull(r, buf)
		if err == io.EOF {
			return nil
		}
		if err == io.ErrUnexpectedEOF {
			return media.Wrap(media.ETruncated, "ingest.raw",
				"input ended mid-packet", nil)
		}
		if err != nil {
			return err
		}
		if buf[0] != 0x47 {
			return media.Wrap(media.EUnrecoverable, "ingest.raw",
				"lost TS sync byte", nil)
		}
		if err := fn(buf); err != nil {
			return err
		}
	}
}

// ReadStream drives fn with every TS packet from r, choosing the framing via
// FramingAuto's one-byte sniff (0x47 means raw TS, anything else is framed
// RTP). The sniffed byte is pushed back internally.
func ReadStream(r io.Reader, framing Framing, fn func([]byte) error) error {
	if framing == FramingRawTS {
		return ReadRawTS(r, fn)
	}
	if framing == FramingRTP {
		return readRTP(r, fn)
	}
	peek := make([]byte, 1)
	if _, err := io.ReadFull(r, peek); err != nil {
		if err == io.ErrUnexpectedEOF || err == io.EOF {
			return nil
		}
		return err
	}
	if peek[0] == 0x47 {
		// Replay the sniffed byte so ReadRawTS starts at a real boundary.
		return ReadRawTS(io.MultiReader(&oneByte{b: peek[0]}, r), fn)
	}
	return readRTP(io.MultiReader(&oneByte{b: peek[0]}, r), fn)
}

// oneByte is a single-byte io.Reader used to replay the sniffed byte.
type oneByte struct {
	b   byte
	got bool
}

func (o *oneByte) Read(p []byte) (int, error) {
	if o.got || len(p) == 0 {
		return 0, io.EOF
	}
	p[0] = o.b
	o.got = true
	return 1, io.EOF
}

func readRTP(r io.Reader, fn func([]byte) error) error {
	rd := NewRTPReader(r)
	for {
		pkts, err := rd.ReadTSPackets()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		for _, p := range pkts {
			if err := fn(p); err != nil {
				return err
			}
		}
	}
}
