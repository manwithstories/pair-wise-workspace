// Package rtp parses length-prefixed RTP/RTCP datagrams from a stream
// and reports recoverable anomalies with the absolute offset of the
// first anomalous byte.
package rtp

import (
	"encoding/binary"
	"fmt"
	"io"

	"archiver/internal/media"
)

const (
	// DefaultMaxPacket is the default per-packet size cap: 1 MiB.
	DefaultMaxPacket = 1 << 20
	lenPrefixLen     = 4
	rtpHeaderLen     = 12
	rtcpSRLen        = 28
)

// SenderReport is a parsed RTCP Sender Report (PT=200).
type SenderReport struct {
	SSRC         uint32
	NTPSec       uint32
	NTPFrac      uint32
	RTPTimestamp uint32
	PacketCount  uint32
	OctetCount   uint32
}

// Packet is one parsed datagram. RTCP packets carry SR; RTP packets
// carry the media fields. Raw always holds the exact original bytes so
// a parse can be round-tripped for verification.
type Packet struct {
	Offset int64 // absolute stream offset of the first header byte
	Raw    []byte
	RTCP   bool

	// RTP fields
	Marker       bool
	PayloadType  uint8
	Seq          uint16
	Timestamp    uint32
	SSRC         uint32
	CSRC         []uint32
	ExtProfile   uint16
	ExtData      []byte
	PaddingBytes []byte
	Payload      []byte

	// RTCP fields
	SR *SenderReport
}

func pktErr(off int64, format string, args ...any) *media.PacketError {
	return &media.PacketError{Offset: off, Reason: fmt.Sprintf(format, args...)}
}

// ParsePacket parses one datagram. abs is the absolute stream offset of
// buf[0]. On failure the error is a *media.PacketError locating the
// first anomalous byte.
func ParsePacket(buf []byte, abs int64) (*Packet, error) {
	if len(buf) < 2 {
		return nil, pktErr(abs+int64(len(buf)), "short header: need 2 bytes, have %d", len(buf))
	}
	if v := buf[0] >> 6; v != 2 {
		return nil, pktErr(abs, "invalid RTP version %d", v)
	}
	pt := buf[1] & 0x7f
	if pt >= 200 && pt <= 204 {
		return parseRTCP(buf, abs)
	}
	return parseRTP(buf, abs)
}

func parseRTP(buf []byte, abs int64) (*Packet, error) {
	if len(buf) < rtpHeaderLen {
		return nil, pktErr(abs+int64(len(buf)), "short header: need %d bytes, have %d", rtpHeaderLen, len(buf))
	}
	cc := int(buf[0] & 0x0f)
	x := buf[0]&0x10 != 0
	pad := buf[0]&0x20 != 0
	p := &Packet{
		Offset:      abs,
		Raw:         buf,
		Marker:      buf[1]&0x80 != 0,
		PayloadType: buf[1] & 0x7f,
		Seq:         binary.BigEndian.Uint16(buf[2:4]),
		Timestamp:   binary.BigEndian.Uint32(buf[4:8]),
		SSRC:        binary.BigEndian.Uint32(buf[8:12]),
	}
	pos := rtpHeaderLen
	if need := pos + 4*cc; len(buf) < need {
		return nil, pktErr(abs+int64(len(buf)), "truncated CSRC list: need %d bytes, have %d", need, len(buf))
	}
	for i := 0; i < cc; i++ {
		p.CSRC = append(p.CSRC, binary.BigEndian.Uint32(buf[pos+4*i:]))
	}
	pos += 4 * cc
	if x {
		if len(buf) < pos+4 {
			return nil, pktErr(abs+int64(len(buf)), "truncated extension header: need %d bytes, have %d", pos+4, len(buf))
		}
		p.ExtProfile = binary.BigEndian.Uint16(buf[pos : pos+2])
		extLen := int(binary.BigEndian.Uint16(buf[pos+2:pos+4])) * 4
		pos += 4
		if len(buf) < pos+extLen {
			return nil, pktErr(abs+int64(len(buf)), "truncated extension: need %d bytes, have %d", pos+extLen, len(buf))
		}
		p.ExtData = buf[pos : pos+extLen]
		pos += extLen
	}
	end := len(buf)
	if pad {
		if end == pos {
			return nil, pktErr(abs+int64(end), "padding flag set but no padding count byte")
		}
		n := int(buf[end-1])
		if n == 0 || end-pos < n {
			return nil, pktErr(abs+int64(end-1), "invalid padding count %d", n)
		}
		p.PaddingBytes = buf[end-n : end]
		end -= n
	}
	p.Payload = buf[pos:end]
	return p, nil
}

func parseRTCP(buf []byte, abs int64) (*Packet, error) {
	if len(buf) < rtcpSRLen {
		return nil, pktErr(abs+int64(len(buf)), "short RTCP sender report: need %d bytes, have %d", rtcpSRLen, len(buf))
	}
	if pt := buf[1]; pt != 200 {
		return nil, pktErr(abs+1, "unsupported RTCP packet type %d", pt)
	}
	p := &Packet{Offset: abs, Raw: buf, RTCP: true, SR: &SenderReport{
		SSRC:         binary.BigEndian.Uint32(buf[4:8]),
		NTPSec:       binary.BigEndian.Uint32(buf[8:12]),
		NTPFrac:      binary.BigEndian.Uint32(buf[12:16]),
		RTPTimestamp: binary.BigEndian.Uint32(buf[16:20]),
		PacketCount:  binary.BigEndian.Uint32(buf[20:24]),
		OctetCount:   binary.BigEndian.Uint32(buf[24:28]),
	}}
	return p, nil
}

// Serialize rebuilds the exact wire bytes of a parsed packet.
func (p *Packet) Serialize() []byte {
	if p.RTCP {
		return append([]byte(nil), p.Raw...) // RTCP fields are not mutated
	}
	out := make([]byte, 0, len(p.Raw))
	b0 := byte(2<<6) | byte(len(p.CSRC))
	if len(p.PaddingBytes) > 0 {
		b0 |= 0x20
	}
	if p.ExtData != nil {
		b0 |= 0x10
	}
	out = append(out, b0)
	b1 := p.PayloadType
	if p.Marker {
		b1 |= 0x80
	}
	out = append(out, b1)
	var tmp [4]byte
	binary.BigEndian.PutUint16(tmp[:2], p.Seq)
	out = append(out, tmp[:2]...)
	binary.BigEndian.PutUint32(tmp[:4], p.Timestamp)
	out = append(out, tmp[:4]...)
	binary.BigEndian.PutUint32(tmp[:4], p.SSRC)
	out = append(out, tmp[:4]...)
	for _, c := range p.CSRC {
		binary.BigEndian.PutUint32(tmp[:4], c)
		out = append(out, tmp[:4]...)
	}
	if p.ExtData != nil {
		binary.BigEndian.PutUint16(tmp[:2], p.ExtProfile)
		out = append(out, tmp[:2]...)
		binary.BigEndian.PutUint16(tmp[:2], uint16(len(p.ExtData)/4))
		out = append(out, tmp[:2]...)
		out = append(out, p.ExtData...)
	}
	out = append(out, p.Payload...)
	out = append(out, p.PaddingBytes...)
	return out
}

// Reader incrementally reads length-prefixed datagrams from a stream.
type Reader struct {
	r   io.Reader
	max int
	off int64
}

func NewReader(r io.Reader, maxPacket int) *Reader {
	if maxPacket <= 0 {
		maxPacket = DefaultMaxPacket
	}
	return &Reader{r: r, max: maxPacket}
}

// Next returns the next packet, io.EOF at a clean stream end, or a
// *media.PacketError for a recoverable anomaly.
func (rd *Reader) Next() (*Packet, error) {
	var hdr [lenPrefixLen]byte
	if _, err := io.ReadFull(rd.r, hdr[:]); err != nil {
		if err == io.EOF {
			return nil, io.EOF
		}
		if err == io.ErrUnexpectedEOF {
			return nil, pktErr(rd.off, "truncated length prefix")
		}
		return nil, err
	}
	base := rd.off
	rd.off += lenPrefixLen
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || int64(n) > int64(rd.max) {
		return nil, pktErr(base, "invalid length prefix %d (max %d)", n, rd.max)
	}
	buf := make([]byte, n)
	got, err := io.ReadFull(rd.r, buf)
	rd.off += int64(got)
	if err != nil {
		return nil, pktErr(rd.off, "truncated payload: need %d bytes, have %d", n, got)
	}
	return ParsePacket(buf, base+lenPrefixLen)
}
