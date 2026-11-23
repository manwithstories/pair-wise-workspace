// Package bitstream scans H.264/H.265/AV1 elementary streams into
// samples with exact source byte ranges, and validates keyframe
// indexes against the scanned stream. Corruption is reported with the
// precise byte offset at which the scanner could no longer accept the
// stream; nothing is silently repaired.
package bitstream

import (
	"encoding/binary"
	"fmt"

	"github.com/pion/rtp"

	"mediatimeline/internal/mediaerr"
)

// Codec identifies the elementary stream format.
type Codec int

const (
	H264 Codec = iota
	H265
	AV1
)

// ParseCodec parses a codec name.
func ParseCodec(s string) (Codec, error) {
	switch s {
	case "h264":
		return H264, nil
	case "h265":
		return H265, nil
	case "av1":
		return AV1, nil
	}
	return 0, fmt.Errorf("unknown codec %q", s)
}

func (c Codec) String() string {
	switch c {
	case H264:
		return "h264"
	case H265:
		return "h265"
	default:
		return "av1"
	}
}

// Sample is one access unit located in the source stream.
type Sample struct {
	Seq      int
	Offset   int64
	Length   int64
	Keyframe bool
}

// Scan parses data as the given codec's elementary stream and returns
// the samples in order. A corrupt stream yields a recoverable
// *mediaerr.Error whose Offset is the first byte the scanner could not
// accept (for truncation, the offset where more data was expected).
func Scan(data []byte, codec Codec, track string) ([]Sample, error) {
	switch codec {
	case H264:
		return scanAnnexB(data, track, false)
	case H265:
		return scanAnnexB(data, track, true)
	default:
		return scanAV1(data, track)
	}
}

// scanAnnexB scans H.264 (hevc=false) or H.265 (hevc=true) byte
// streams framed by 0x000001 / 0x00000001 start codes.
func scanAnnexB(data []byte, track string, hevc bool) ([]Sample, error) {
	codecName := "h264"
	hdrLen := int64(1)
	if hevc {
		codecName = "h265"
		hdrLen = 2
	}
	// Locate all start code prefixes and the NAL header positions.
	type nal struct{ prefix, header int64 }
	var nals []nal
	for i := 0; i+3 <= len(data); i++ {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			nals = append(nals, nal{prefix: int64(i), header: int64(i + 3)})
			i += 2
		} else if i+4 <= len(data) && data[i] == 0 && data[i+1] == 0 && data[i+2] == 0 && data[i+3] == 1 {
			nals = append(nals, nal{prefix: int64(i), header: int64(i + 4)})
			i += 3
		}
	}
	if len(nals) == 0 {
		return nil, mediaerr.New(mediaerr.Scan, track, 0, mediaerr.NA, true,
			"no %s start codes found", codecName)
	}
	if nals[0].prefix != 0 {
		return nil, mediaerr.New(mediaerr.Scan, track, 0, mediaerr.NA, true,
			"%s stream does not start with a start code", codecName)
	}
	samples := make([]Sample, 0, len(nals))
	for i, n := range nals {
		end := int64(len(data))
		if i+1 < len(nals) {
			end = nals[i+1].prefix
		}
		if n.header+hdrLen > end {
			return nil, mediaerr.New(mediaerr.Scan, track, n.prefix, mediaerr.NA, true,
				"truncated %s NAL unit: header incomplete", codecName)
		}
		if end-n.header < hdrLen+2 {
			return nil, mediaerr.New(mediaerr.Scan, track, n.prefix, mediaerr.NA, true,
				"truncated %s NAL unit: payload too short", codecName)
		}
		key, err := checkNALHeader(data[n.header:n.header+hdrLen], n.header, hevc, track)
		if err != nil {
			return nil, err
		}
		samples = append(samples, Sample{Seq: i, Offset: n.prefix, Length: end - n.prefix, Keyframe: key})
	}
	return samples, nil
}

// checkNALHeader validates the NAL header and reports keyframe-ness.
func checkNALHeader(h []byte, off int64, hevc bool, track string) (bool, error) {
	if h[0]&0x80 != 0 {
		return false, mediaerr.New(mediaerr.Scan, track, off, mediaerr.NA, true,
			"forbidden_zero_bit set in NAL header")
	}
	if !hevc {
		t := h[0] & 0x1f
		if t == 0 || t > 23 {
			return false, mediaerr.New(mediaerr.Scan, track, off, mediaerr.NA, true,
				"invalid h264 nal_unit_type %d", t)
		}
		return t == 5, nil
	}
	t := (h[0] >> 1) & 0x3f
	layer := (int64(h[0]&0x01) << 5) | int64(h[1]>>3)
	if layer != 0 {
		return false, mediaerr.New(mediaerr.Scan, track, off, mediaerr.NA, true,
			"h265 nuh_layer_id %d != 0", layer)
	}
	if t > 40 {
		return false, mediaerr.New(mediaerr.Scan, track, off, mediaerr.NA, true,
			"invalid h265 nal_unit_type %d", t)
	}
	return t >= 16 && t <= 21, nil
}

// scanAV1 scans an AV1 low-overhead OBU stream. Every OBU must carry
// a leb128 size field, which makes truncation precisely locatable.
func scanAV1(data []byte, track string) ([]Sample, error) {
	var samples []Sample
	pos := int64(0)
	for pos < int64(len(data)) {
		start := pos
		h := data[pos]
		pos++
		if h&0x80 != 0 {
			return nil, mediaerr.New(mediaerr.Scan, track, start, mediaerr.NA, true,
				"obu_forbidden_bit set")
		}
		typ := (h >> 3) & 0x0f
		if typ == 0 || typ > 15 {
			return nil, mediaerr.New(mediaerr.Scan, track, start, mediaerr.NA, true,
				"invalid obu_type %d", typ)
		}
		if h&0x04 != 0 { // extension flag
			if pos >= int64(len(data)) {
				return nil, mediaerr.New(mediaerr.Scan, track, pos, mediaerr.NA, true,
					"truncated obu extension header")
			}
			pos++
		}
		if h&0x02 == 0 {
			return nil, mediaerr.New(mediaerr.Scan, track, start, mediaerr.NA, true,
				"obu without size field is not supported")
		}
		size, n := parseLeb128(data[pos:])
		if n == 0 {
			return nil, mediaerr.New(mediaerr.Scan, track, int64(len(data)), mediaerr.NA, true,
				"truncated leb128 obu size")
		}
		pos += int64(n)
		if pos+int64(size) > int64(len(data)) {
			return nil, mediaerr.New(mediaerr.Scan, track, int64(len(data)), mediaerr.NA, true,
				"truncated obu payload: want %d bytes, have %d", size, int64(len(data))-pos)
		}
		pos += int64(size)
		// A frame-header OBU (type 3) marks a keyframe in this pipeline.
		samples = append(samples, Sample{Seq: len(samples), Offset: start, Length: pos - start, Keyframe: typ == 3})
	}
	if len(samples) == 0 {
		return nil, mediaerr.New(mediaerr.Scan, track, 0, mediaerr.NA, true, "empty av1 stream")
	}
	return samples, nil
}

// parseLeb128 parses an unsigned leb128 value; n==0 means the buffer
// ended mid-value.
func parseLeb128(b []byte) (v uint64, n int) {
	for i := 0; i < 8 && i < len(b); i++ {
		v |= uint64(b[i]&0x7f) << (7 * i)
		if b[i]&0x80 == 0 {
			return v, i + 1
		}
	}
	return 0, 0
}

// RTPSample is one depacketised sample with its raw RTP timestamps.
type RTPSample struct {
	Offset    int64
	Length    int64
	RawTS     uint64 // raw 32-bit RTP timestamp
	RawSeq    uint64 // raw 16-bit RTP sequence number
	LostBefore int   // packets lost before this sample (sequence gap)
}

// DepacketizeRTP reads a length-prefixed RTP packet capture (4-byte
// big-endian length + marshalled RTP packet per record), concatenates
// the payloads into an elementary stream and returns it together with
// per-sample RTP metadata. Sequence numbers are extended through the
// 16-bit wrap; gaps are reported per sample.
func DepacketizeRTP(data []byte, track string) ([]byte, []RTPSample, error) {
	var es []byte
	var out []RTPSample
	var lastExtSeq int64
	seqWrap := seqExt{}
	for off := 0; off < len(data); {
		if off+4 > len(data) {
			return nil, nil, mediaerr.New(mediaerr.Scan, track, int64(off), mediaerr.NA, true,
				"truncated rtp record length")
		}
		n := int(binary.BigEndian.Uint32(data[off:]))
		if off+4+n > len(data) {
			return nil, nil, mediaerr.New(mediaerr.Scan, track, int64(len(data)), mediaerr.NA, true,
				"truncated rtp packet: want %d bytes", n)
		}
		var pkt rtp.Packet
		if err := pkt.Unmarshal(data[off+4 : off+4+n]); err != nil {
			return nil, nil, mediaerr.New(mediaerr.Scan, track, int64(off+4), mediaerr.NA, true,
				"rtp unmarshal: %v", err)
		}
		ext := seqWrap.extend(uint64(pkt.SequenceNumber))
		lost := 0
		if len(out) > 0 {
			if gap := ext - lastExtSeq - 1; gap > 0 {
				lost = int(gap)
			}
		}
		lastExtSeq = ext
		out = append(out, RTPSample{
			Offset: int64(len(es)), Length: int64(len(pkt.Payload)),
			RawTS: uint64(pkt.Timestamp), RawSeq: uint64(pkt.SequenceNumber),
			LostBefore: lost,
		})
		es = append(es, pkt.Payload...)
		off += 4 + n
	}
	if len(out) == 0 {
		return nil, nil, mediaerr.New(mediaerr.Scan, track, 0, mediaerr.NA, true, "no rtp packets")
	}
	return es, out, nil
}

// seqExt extends 16-bit RTP sequence numbers across the wrap.
type seqExt struct {
	inited bool
	last   uint64
	ext    int64
}

func (s *seqExt) extend(v uint64) int64 {
	const mod = 1 << 16
	if !s.inited {
		s.inited, s.last, s.ext = true, v, int64(v)
		return s.ext
	}
	d := (v - s.last) & (mod - 1)
	if d >= mod/2 {
		s.ext -= int64(mod - d)
	} else {
		s.ext += int64(d)
	}
	s.last = v
	return s.ext
}
