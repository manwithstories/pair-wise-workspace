package ts

import (
	"encoding/binary"

	"tsqc/internal/clock"
)

// PESHeader is the small part of a PES packet header needed for demuxing.
type PESHeader struct {
	StreamID  uint8
	PacketLen int
	HeaderLen int
	PTS       int64
	PTSRaw    uint64
	DTS       int64
	DTSRaw    uint64
	HasPTS    bool
	HasDTS    bool
	DataStart int
}

// ParsePES decodes start code, stream id and optional PTS/DTS. Timestamps are
// returned as raw 33-bit values; the caller unwraps them per stream.
func ParsePES(b []byte) (*PESHeader, error) {
	if len(b) < 9 {
		return nil, clock.FatalError(clock.CodeTruncated, "ts.ParsePES", "header needs 9 bytes, have %d", len(b))
	}
	if b[0] != 0 || b[1] != 0 || b[2] != 1 {
		return nil, clock.RecoverableError(clock.CodeTruncated, "ts.ParsePES", "bad start code % x", b[:3])
	}
	h := &PESHeader{StreamID: b[3], PacketLen: int(binary.BigEndian.Uint16(b[4:6]))}
	if h.StreamID == 0xBC || h.StreamID == 0xBE || h.StreamID == 0xBF || h.StreamID == 0xF0 || h.StreamID == 0xF1 || h.StreamID == 0xF2 || h.StreamID == 0xF8 || h.StreamID == 0xFF {
		h.DataStart = 6
		return h, nil
	}
	if len(b) < 9 {
		return nil, clock.FatalError(clock.CodeTruncated, "ts.ParsePES", "missing PES extension")
	}
	flags := b[7]
	hlen := int(b[8])
	h.HeaderLen = hlen
	if 9+hlen > len(b) {
		return nil, clock.FatalError(clock.CodeTruncated, "ts.ParsePES", "PES header overruns")
	}
	off := 9
	if flags&0x80 != 0 {
		raw, n, err := parseStamp(b[off:])
		if err != nil {
			return nil, err
		}
		h.PTSRaw = raw
		h.PTS = int64(raw)
		h.HasPTS = true
		off += n
	}
	if flags&0x40 != 0 {
		raw, n, err := parseStamp(b[off:])
		if err != nil {
			return nil, err
		}
		h.DTSRaw = raw
		h.DTS = int64(raw)
		h.HasDTS = true
		off += n
	}
	h.DataStart = 9 + hlen
	return h, nil
}

func parseStamp(b []byte) (uint64, int, error) {
	if len(b) < 5 {
		return 0, 0, clock.FatalError(clock.CodeTruncated, "ts.parseStamp", "short timestamp")
	}
	if b[0]&0x01 == 0 {
		return 0, 0, clock.RecoverableError(clock.CodeTruncated, "ts.parseStamp", "timestamp marker bits not set")
	}
	v := uint64(b[0]>>1&0x07)<<30 | uint64(b[1])<<22 | uint64(b[2]>>1)<<15 | uint64(b[3])<<7 | uint64(b[4]>>1)
	return v, 5, nil
}
