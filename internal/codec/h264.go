package codec

import (
	"encoding/binary"

	"mediad/internal/fault"
)

// NAL is a located H.264 network abstraction layer unit.
type NAL struct {
	Index  int    `json:"index"`
	Offset int64  `json:"offset"`
	Length int    `json:"length"`
	Type   uint8  `json:"type"`
	IDR    bool   `json:"idr"`
}

// KeyframeHit compares an expected keyframe ordinal against scanned NAL indices.
type KeyframeHit struct {
	ExpectedOrdinal int   `json:"expected_ordinal"`
	ExpectedIndex   int   `json:"expected_index"`
	ActualIndex     int   `json:"actual_index"`
	Matched         bool  `json:"matched"`
}

// ScanAnnexB scans start-code delimited H.264 NAL units.
func ScanAnnexB(path string, data []byte) ([]NAL, error) {
	type start struct {
		offset     int
		headerSize int
	}
	var starts []start
	for i := 0; i+2 < len(data); i++ {
		if i+3 < len(data) && data[i] == 0 && data[i+1] == 0 && data[i+2] == 0 && data[i+3] == 1 {
			starts = append(starts, start{offset: i, headerSize: 4})
			i += 3
		} else if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			starts = append(starts, start{offset: i, headerSize: 3})
			i += 2
		}
	}
	if len(starts) == 0 {
		return nil, fault.New(fault.MdatCorrupt, path, 0, "no Annex-B start codes found")
	}
	out := make([]NAL, 0, len(starts))
	for i, s := range starts {
		payload := s.offset + s.headerSize
		end := len(data)
		if i+1 < len(starts) {
			end = starts[i+1].offset
		}
		if payload >= end {
			return nil, fault.New(fault.MdatCorrupt, path, int64(s.offset), "empty NAL unit")
		}
		hdr := data[payload]
		nalType := hdr & 0x1f
		out = append(out, NAL{
			Index: i, Offset: int64(s.offset), Length: end - s.offset,
			Type: nalType, IDR: nalType == 5,
		})
	}
	return out, nil
}

// ScanAVCC scans 4-byte length-prefixed NAL units. The caller supplies the
// enclosing mdat offset for precise fault location.
func ScanAVCC(path string, data []byte, baseOffset int64) ([]NAL, error) {
	var out []NAL
	pos := 0
	idx := 0
	for pos < len(data) {
		if len(data)-pos < 4 {
			return nil, fault.New(fault.MdatCorrupt, path, baseOffset+int64(pos), "short AVCC length")
		}
		n := int(binary.BigEndian.Uint32(data[pos : pos+4]))
		if n <= 0 || pos+4+n > len(data) {
			return nil, fault.New(fault.MdatCorrupt, path, baseOffset+int64(pos), "invalid AVCC NAL length %d", n)
		}
		hdr := data[pos+4]
		nalType := hdr & 0x1f
		out = append(out, NAL{
			Index: idx, Offset: baseOffset + int64(pos), Length: 4 + n,
			Type: nalType, IDR: nalType == 5,
		})
		pos += 4 + n
		idx++
	}
	return out, nil
}

// KeyframeIndices returns NAL indices containing IDR pictures.
func KeyframeIndices(nals []NAL) []int {
	var out []int
	for _, n := range nals {
		if n.IDR {
			out = append(out, n.Index)
		}
	}
	return out
}

// ValidateKeyframeIndex checks that actual keyframe indices equal expected.
func ValidateKeyframeIndex(path string, actual, expected []int) ([]KeyframeHit, error) {
	hits := make([]KeyframeHit, len(expected))
	allMatch := len(actual) == len(expected)
	for i, want := range expected {
		got := -1
		if i < len(actual) {
			got = actual[i]
		}
		match := got == want
		hits[i] = KeyframeHit{ExpectedOrdinal: i + 1, ExpectedIndex: want, ActualIndex: got, Matched: match}
		if !match {
			allMatch = false
		}
	}
	if len(actual) != len(expected) {
		allMatch = false
	}
	if !allMatch {
		return hits, fault.New(fault.Boundary, path, -1,
			"keyframe index mismatch: actual=%v expected=%v", actual, expected)
	}
	return hits, nil
}

// KeyframeScore returns matched/total as a 0..1 hit rate.
func KeyframeScore(hits []KeyframeHit) (int, int, float64) {
	matched := 0
	for _, h := range hits {
		if h.Matched {
			matched++
		}
	}
	total := len(hits)
	if total == 0 {
		return 0, 0, 0
	}
	return matched, total, float64(matched) / float64(total)
}
