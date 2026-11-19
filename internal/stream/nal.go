// Package stream scans concatenated access units in Annex B (H.264/H.265) and
// AV1 low-overhead OBU form. It reports every unit's byte offset and type and
// classifies the structural failures that make a later index untrustworthy.
package stream

import (
	"fmt"
	"sort"

	"archive.local/recover/internal/errs"
)

// Codec selects the framing rules.
type Codec string

const (
	H264 Codec = "h264"
	H265 Codec = "h265"
	AV1  Codec = "av1"
)

// Limits bound the work a single scan may do on hostile input.
const (
	MaxUnits     = 2_000_000
	MaxScanBytes = 512 << 20
	MaxNALSize   = 16 << 20
	MaxOBUCount  = 1_000_000
)

// Unit is one codec-level unit (NAL unit or OBU).
type Unit struct {
	Index    int   `json:"index"`
	Offset   int64 `json:"offset"`    // byte offset of the start code / OBU header
	HeaderAt int64 `json:"header_at"` // byte offset of the unit header
	Length   int   `json:"length"`    // bytes incl. start code for Annex B
	Type     int   `json:"type"`      // NAL type or OBU type
	Keyframe bool  `json:"keyframe"`
	Codec    Codec `json:"codec"`
	// Pending marks a unit whose header could be parsed but whose framing may
	// be untrustworthy (for example a truncated final unit).
	Pending bool `json:"pending"`
}

// Result is the ordered unit list plus classification.
type Result struct {
	Codec     Codec     `json:"codec"`
	Units     []Unit    `json:"units"`
	Issues    errs.List `json:"issues"`
	Bytes     int       `json:"bytes"`
	Truncated bool      `json:"truncated"`
	Keyframes []int     `json:"keyframes"` // indexes into Units
}

// Scan dispatches on codec.
func Scan(codec Codec, data []byte) (*Result, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty bitstream")
	}
	if len(data) > MaxScanBytes {
		return nil, fmt.Errorf("bitstream %d bytes exceeds limit %d", len(data), MaxScanBytes)
	}
	switch codec {
	case H264:
		return scanAnnexB(codec, data, 1)
	case H265:
		return scanAnnexB(codec, data, 2)
	case AV1:
		return scanAV1(data)
	default:
		return nil, fmt.Errorf("unsupported codec %q", codec)
	}
}

// scanAnnexB handles H.264 (1-byte NAL header) and H.265 (2-byte NAL header).
func scanAnnexB(codec Codec, data []byte, headerLen int) (*Result, error) {
	res := &Result{Codec: codec, Bytes: len(data)}
	loc := func(off int64) errs.Location {
		return errs.Location{Offset: off, Sample: -1}
	}
	starts := findStartCodes(data)
	if len(starts) == 0 {
		res.Issues.Add(errs.ClassCorrupt, errs.CodeNALStartCodeTruncated,
			"no Annex B start code found", errs.Location{Offset: 0, Sample: -1})
		res.Truncated = true
		return res, nil
	}
	if starts[0].offset > 0 {
		res.Issues.Add(errs.ClassCorrupt, errs.CodeNALStartCodeTruncated,
			fmt.Sprintf("%d leading bytes before first start code", starts[0].offset), loc(0))
	}
	for i, s := range starts {
		end := int64(len(data))
		if i+1 < len(starts) {
			end = starts[i+1].offset
		}
		payloadAt := s.offset + int64(s.length)
		if payloadAt >= end {
			res.Issues.Add(errs.ClassTruncated, errs.CodeNALHeaderTruncated,
				"start code with no NAL header", loc(s.offset))
			res.Truncated = true
			if len(res.Units) < MaxUnits {
				res.Units = append(res.Units, Unit{
					Index: len(res.Units), Offset: s.offset, HeaderAt: payloadAt,
					Length: int(end - s.offset), Codec: codec, Pending: true,
				})
			}
			continue
		}
		if end-payloadAt < int64(headerLen) {
			res.Issues.Add(errs.ClassTruncated, errs.CodeNALHeaderTruncated,
				fmt.Sprintf("NAL header needs %d bytes, only %d available", headerLen, end-payloadAt), loc(s.offset))
			res.Truncated = true
			// Keep the unit as pending: the index may still point at it.
			if len(res.Units) < MaxUnits {
				res.Units = append(res.Units, Unit{
					Index: len(res.Units), Offset: s.offset, HeaderAt: payloadAt,
					Length: int(end - s.offset), Codec: codec, Pending: true,
				})
			}
			continue
		}
		length := int(end - s.offset)
		if length > MaxNALSize {
			res.Issues.Add(errs.ClassBadLength, errs.CodeNALLengthOverrun,
				fmt.Sprintf("NAL length %d exceeds limit %d", length, MaxNALSize), loc(s.offset))
			return res, nil
		}
		nalType := int(data[payloadAt] & 0x1f)
		if codec == H265 {
			nalType = int((data[payloadAt] >> 1) & 0x3f)
		}
		if nalType == 0 {
			res.Issues.Add(errs.ClassCorrupt, errs.CodeNALLengthZero,
				"NAL type 0 is invalid in a decodable stream", loc(s.offset))
		}
		u := Unit{
			Index: len(res.Units), Offset: s.offset, HeaderAt: payloadAt,
			Length: length, Type: nalType, Codec: codec,
		}
		u.Keyframe = isKeyframeNAL(codec, nalType)
		if u.Keyframe {
			res.Keyframes = append(res.Keyframes, u.Index)
		}
		if len(res.Units) < MaxUnits {
			res.Units = append(res.Units, u)
		} else {
			res.Issues.Add(errs.ClassCorrupt, errs.CodeNALLengthOverrun,
				fmt.Sprintf("unit count exceeds limit %d", MaxUnits), loc(s.offset))
			break
		}
	}
	return res, nil
}

// isKeyframeNAL reports whether a NAL type marks a random-access point.
func isKeyframeNAL(codec Codec, t int) bool {
	if codec == H264 {
		// IDR slices; 5 is the canonical IDR type.
		return t == 5
	}
	// HEVC: IRAP pictures 16..21 (BLA/IDR/CRA).
	return t >= 16 && t <= 21
}

type startCode struct {
	offset int64
	length int
}

// findStartCodes locates 3- and 4-byte Annex B start codes. A four-byte code
// is preferred whenever it is available so that a zero run is not split.
func findStartCodes(data []byte) []startCode {
	var out []startCode
	n := len(data)
	i := 0
	for i < n {
		if i+3 < n && data[i] == 0 && data[i+1] == 0 && data[i+2] == 0 && data[i+3] == 1 {
			out = append(out, startCode{offset: int64(i), length: 4})
			i += 4
			continue
		}
		if i+2 < n && data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			out = append(out, startCode{offset: int64(i), length: 3})
			i += 3
			continue
		}
		i++
	}
	return out
}

// AV1 OBU types (low-overhead bitstream format).
const (
	obuSequenceHeader = 1
	obuFrame          = 6
	obuFrameHeader    = 3
	obuTileGroup      = 4
	obuTemporalDelim  = 2
)

// scanAV1 walks OBUs using the leb128 size field.
func scanAV1(data []byte) (*Result, error) {
	res := &Result{Codec: AV1, Bytes: len(data)}
	off := 0
	for off < len(data) {
		if len(res.Units) >= MaxUnits {
			res.Issues.Add(errs.ClassCorrupt, errs.CodeOBULengthOverrun,
				fmt.Sprintf("OBU count exceeds limit %d", MaxUnits), errs.Location{Offset: int64(off), Sample: -1})
			break
		}
		if off+1 > len(data) {
			res.Issues.Add(errs.ClassTruncated, errs.CodeOBUHeaderTruncated,
				"OBU header byte missing", errs.Location{Offset: int64(off), Sample: -1})
			res.Truncated = true
			break
		}
		hdr := data[off]
		if hdr&0x80 != 0 {
			// forbidden bit set: not a valid low-overhead OBU stream.
			res.Issues.Add(errs.ClassCorrupt, errs.CodeOBUHeaderTruncated,
				"OBU forbidden bit set", errs.Location{Offset: int64(off), Sample: -1})
			break
		}
		obuType := int((hdr >> 3) & 0x0f)
		hasExt := hdr&0x04 != 0
		hasSize := hdr&0x02 != 0
		hdrLen := 1
		if hasExt {
			hdrLen++
		}
		if off+hdrLen > len(data) {
			res.Issues.Add(errs.ClassTruncated, errs.CodeOBUHeaderTruncated,
				"OBU extension flag without extension byte", errs.Location{Offset: int64(off), Sample: -1})
			res.Truncated = true
			break
		}
		p := off + hdrLen
		if !hasSize {
			res.Issues.Add(errs.ClassCorrupt, errs.CodeOBULengthZero,
				"OBU without size field cannot be framed", errs.Location{Offset: int64(off), Sample: -1})
			break
		}
		size, n, ok := readLeb128(data[p:])
		if !ok {
			res.Issues.Add(errs.ClassTruncated, errs.CodeOBUHeaderTruncated,
				"truncated LEB128 OBU size", errs.Location{Offset: int64(off), Sample: -1})
			res.Truncated = true
			break
		}
		p += n
		if size == 0 && obuType != obuTemporalDelim {
			res.Issues.Add(errs.ClassCorrupt, errs.CodeOBULengthZero,
				"OBU payload size is zero", errs.Location{Offset: int64(off), Sample: -1})
			break
		}
		if size > uint64(len(data)-p) {
			res.Issues.Add(errs.ClassBadLength, errs.CodeOBULengthOverrun,
				fmt.Sprintf("OBU declares %d payload bytes, %d remain", size, len(data)-p),
				errs.Location{Offset: int64(off), Sample: -1})
			res.Truncated = true
			break
		}
		u := Unit{
			Index: len(res.Units), Offset: int64(off), HeaderAt: int64(off),
			Length: p + int(size) - off, Type: obuType, Codec: AV1,
			Keyframe: obuType == obuFrame || obuType == obuTileGroup,
		}
		if u.Keyframe {
			res.Keyframes = append(res.Keyframes, u.Index)
		}
		res.Units = append(res.Units, u)
		off = p + int(size)
	}
	return res, nil
}

// readLeb128 decodes an AV1 length field, bounded to 8 bytes.
func readLeb128(b []byte) (uint64, int, bool) {
	var v uint64
	for i := 0; i < len(b) && i < 8; i++ {
		v |= uint64(b[i]&0x7f) << (7 * uint(i))
		if b[i]&0x80 == 0 {
			return v, i + 1, true
		}
	}
	return 0, 0, false
}

// KeyframeOffsets returns the byte offsets for every keyframe unit, ascending.
func (r *Result) KeyframeOffsets() []int64 {
	out := make([]int64, 0, len(r.Keyframes))
	for _, i := range r.Keyframes {
		out = append(out, r.Units[i].Offset)
	}
	sort.Slice(out, func(a, b int) bool { return out[a] < out[b] })
	return out
}
