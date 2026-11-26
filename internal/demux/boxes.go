// Package demux parses CMAF/fMP4 init and media segments using go-mp4,
// reporting the first error offset when boxes are out of bounds or
// truncated so the affected segment can be isolated.
package demux

import (
	"bytes"
	"fmt"
	"io"

	mp4 "github.com/abema/go-mp4"

	"relaycheck/internal/clock"
)

// Guard limits for hostile or corrupt inputs.
const (
	MaxBoxSize        = 1 << 26 // 64 MiB per box
	MaxSegmentSize    = 1 << 28 // 256 MiB per segment file
	MaxSamplesPerTrun = 1 << 20 // sample-table limit
	maxBoxDepth       = 8
)

// TrackInfo describes one track found in an init segment.
type TrackInfo struct {
	ID        uint32
	Handler   string // "vide", "soun", "text", ...
	Codec     string // "avc1", "hvc1", "av01", "mp4a", "Opus", "wvtt", ...
	Timescale uint32
	Language  string
	Channels  uint16
	Width     uint16
	Height    uint16
}

// InitInfo is the parsed result of an init segment.
type InitInfo struct {
	Tracks []TrackInfo
}

// TrackFrag is one parsed traf of a media segment.
type TrackFrag struct {
	TrackID       uint32
	HasTFDT       bool
	TFDT          uint64
	Entries       []mp4.TrunEntry
	TrunFlags     uint32
	DataOffset    int32
	HasDataOffset bool
	MoofOffset    int64
}

// Segment is a parsed media segment.
type Segment struct {
	Sequence    uint32
	HasSequence bool
	MoofOffset  int64
	MdatOffset  int64 // absolute offset of the mdat payload
	Mdat        []byte
	Traf        []TrackFrag
}

var containerTypes = map[mp4.BoxType]bool{}

func init() {
	for _, t := range []mp4.BoxType{
		mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(),
		mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeMoof(),
		mp4.BoxTypeTraf(), mp4.BoxTypeEdts(), mp4.BoxTypeDinf(),
	} {
		containerTypes[t] = true
	}
}

// scanRegion walks box headers in data[start:end] and returns the offset of
// the first header that is truncated, out of bounds, oversized or nested
// too deep. It uses go-mp4's ReadBoxInfo for header parsing.
func scanRegion(data []byte, start, end uint64, depth int) (int64, bool) {
	if depth > maxBoxDepth {
		return int64(start), true
	}
	r := bytes.NewReader(data)
	pos := start
	for pos < end {
		if end-pos < 8 {
			return int64(pos), true // truncated box header
		}
		if _, err := r.Seek(int64(pos), io.SeekStart); err != nil {
			return int64(pos), true
		}
		bi, err := mp4.ReadBoxInfo(r)
		if err != nil {
			return int64(pos), true
		}
		size := bi.Size
		if bi.ExtendToEOF {
			size = end - pos
		}
		if size < bi.HeaderSize || size > MaxBoxSize || pos+size > end {
			return int64(pos), true
		}
		child := pos + bi.HeaderSize
		if bi.Type == mp4.BoxTypeStsd() {
			child += 8 // FullBox header + entry count
		} else if !containerTypes[bi.Type] {
			child = 0
		}
		if child != 0 {
			if child > pos+size {
				return int64(pos), true
			}
			if off, bad := scanRegion(data, child, pos+size, depth+1); bad {
				return off, true
			}
		}
		pos += size
	}
	return 0, false
}

// firstBoxError locates the first out-of-bounds or truncated box header.
func firstBoxError(data []byte) (int64, bool) {
	return scanRegion(data, 0, uint64(len(data)), 0)
}

// DecodeLanguage converts the packed 5-bit ISO-639-2/T triplet to text.
func DecodeLanguage(l [3]byte) string {
	b := []byte{l[0], l[1], l[2]}
	for i, c := range b {
		if c >= 1 && c <= 26 {
			b[i] = c + 0x60
		}
	}
	return string(b)
}

// EncodeLanguage packs a 3-letter language code into 5-bit triplet form.
func EncodeLanguage(s string) [3]byte {
	var l [3]byte
	for i := 0; i < 3 && i < len(s); i++ {
		c := s[i]
		if c >= 0x61 && c <= 0x7a {
			l[i] = c - 0x60
		}
	}
	return l
}

// ParseSegment parses one media segment held fully in memory. A non-nil
// fault describes the first structural error; the segment is then nil and
// must be isolated by the caller.
func ParseSegment(data []byte, seq uint32) (*Segment, *clock.Fault) {
	if len(data) > MaxSegmentSize {
		f := clock.NewFault(clock.FaultBoxBound, seq, 0, 0, "segment exceeds size limit")
		return nil, &f
	}
	if off, bad := firstBoxError(data); bad {
		f := clock.NewFault(clock.FaultTruncate, seq, 0, off, "box truncated or out of bounds")
		return nil, &f
	}
	seg := &Segment{MoofOffset: -1, MdatOffset: -1}
	var cur *TrackFrag
	_, err := mp4.ReadBoxStructure(bytes.NewReader(data), func(h *mp4.ReadHandle) (interface{}, error) {
		switch h.BoxInfo.Type {
		case mp4.BoxTypeMoof():
			seg.MoofOffset = int64(h.BoxInfo.Offset)
			return h.Expand()
		case mp4.BoxTypeMfhd():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			seg.Sequence = box.(*mp4.Mfhd).SequenceNumber
			seg.HasSequence = true
		case mp4.BoxTypeTraf():
			seg.Traf = append(seg.Traf, TrackFrag{MoofOffset: seg.MoofOffset})
			cur = &seg.Traf[len(seg.Traf)-1]
			return h.Expand()
		case mp4.BoxTypeTfhd():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			cur.TrackID = box.(*mp4.Tfhd).TrackID
		case mp4.BoxTypeTfdt():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			cur.TFDT = box.(*mp4.Tfdt).GetBaseMediaDecodeTime()
			cur.HasTFDT = true
		case mp4.BoxTypeTrun():
			off := h.BoxInfo.Offset + h.BoxInfo.HeaderSize
			if uint64(len(data)) < off+8 {
				return nil, fmt.Errorf("trun header truncated")
			}
			count := uint32(data[off+4])<<24 | uint32(data[off+5])<<16 |
				uint32(data[off+6])<<8 | uint32(data[off+7])
			if count > MaxSamplesPerTrun {
				return nil, fmt.Errorf("trun sample count %d exceeds limit", count)
			}
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			trun := box.(*mp4.Trun)
			cur.Entries = trun.Entries
			cur.TrunFlags = trun.GetFlags()
			cur.DataOffset = trun.DataOffset
			cur.HasDataOffset = trun.GetFlags()&0x1 != 0
		case mp4.BoxTypeMdat():
			seg.MdatOffset = int64(h.BoxInfo.Offset + h.BoxInfo.HeaderSize)
			buf := new(bytes.Buffer)
			if _, err := h.ReadData(buf); err != nil {
				return nil, err
			}
			seg.Mdat = buf.Bytes()
		}
		return nil, nil
	})
	if err != nil {
		f := clock.NewFault(clock.FaultTruncate, seq, 0, -1, err.Error())
		return nil, &f
	}
	if seg.MoofOffset < 0 || !seg.HasSequence {
		f := clock.NewFault(clock.FaultTruncate, seq, 0, 0, "missing moof or mfhd")
		return nil, &f
	}
	return seg, nil
}
