package audit

import (
	"encoding/binary"
	"fmt"

	"mediqc/internal/media"
)

// FMP4Sample is one independently extracted fragmented-MP4 sample.
type FMP4Sample struct {
	TrackID  uint32
	Duration uint32
	Data     []byte
}

type mp4Box struct {
	typ          string
	start        int
	payloadStart int
	end          int
}

type trunInfo struct {
	trackID    uint32
	dataOffset int32
	durations  []uint32
	sizes      []uint32
}

// ExtractFMP4Samples parses moof/tfhd/trun and reads sample bytes via data offsets.
func ExtractFMP4Samples(data []byte) ([]FMP4Sample, *media.Error) {
	boxes, err := topBoxes(data, 0, len(data))
	if err != nil {
		return nil, auditError(err.Error())
	}
	var out []FMP4Sample
	for _, b := range boxes {
		if b.typ != "moof" {
			continue
		}
		truns, err := parseMoof(data, b)
		if err != nil {
			return nil, auditError(err.Error())
		}
		for _, tr := range truns {
			pos := b.start + int(tr.dataOffset)
			if pos < 0 || pos > len(data) {
				return nil, auditError("fMP4 trun data offset out of range")
			}
			for i, size := range tr.sizes {
				if size > uint32(len(data)-pos) {
					return nil, auditError("fMP4 sample size overruns file")
				}
				var dur uint32
				if i < len(tr.durations) {
					dur = tr.durations[i]
				}
				out = append(out, FMP4Sample{TrackID: tr.trackID, Duration: dur, Data: data[pos : pos+int(size)]})
				pos += int(size)
			}
		}
	}
	if len(out) == 0 {
		return nil, auditError("fMP4 contains no trun samples")
	}
	return out, nil
}

func topBoxes(data []byte, start, end int) ([]mp4Box, error) {
	var boxes []mp4Box
	pos := start
	for pos+8 <= end {
		size32 := binary.BigEndian.Uint32(data[pos : pos+4])
		header := 8
		size := int64(size32)
		if size32 == 1 {
			if pos+16 > end {
				return nil, fmt.Errorf("large box header truncated")
			}
			size = int64(binary.BigEndian.Uint64(data[pos+8 : pos+16]))
			header = 16
		} else if size32 == 0 {
			size = int64(end - pos)
		}
		if size < int64(header) || pos+int(size) > end {
			return nil, fmt.Errorf("invalid MP4 box size at %d", pos)
		}
		boxes = append(boxes, mp4Box{
			typ:          string(data[pos+4 : pos+8]),
			start:        pos,
			payloadStart: pos + header,
			end:          pos + int(size),
		})
		pos += int(size)
	}
	return boxes, nil
}

func parseMoof(data []byte, moof mp4Box) ([]trunInfo, error) {
	children, err := topBoxes(data, moof.payloadStart, moof.end)
	if err != nil {
		return nil, err
	}
	var truns []trunInfo
	for _, child := range children {
		if child.typ != "traf" {
			continue
		}
		trafChildren, err := topBoxes(data, child.payloadStart, child.end)
		if err != nil {
			return nil, err
		}
		var info trunInfo
		var hasTrun bool
		for _, tb := range trafChildren {
			switch tb.typ {
			case "tfhd":
				if tb.payloadStart+8 > tb.end {
					return nil, fmt.Errorf("short tfhd")
				}
				info.trackID = binary.BigEndian.Uint32(data[tb.payloadStart+4 : tb.payloadStart+8])
			case "trun":
				parsed, perr := parseTrun(data, tb)
				if perr != nil {
					return nil, perr
				}
				info.dataOffset = parsed.dataOffset
				info.durations = parsed.durations
				info.sizes = parsed.sizes
				hasTrun = true
			}
		}
		if hasTrun {
			truns = append(truns, info)
		}
	}
	return truns, nil
}

func parseTrun(data []byte, b mp4Box) (trunInfo, error) {
	p := b.payloadStart
	if p+8 > b.end {
		return trunInfo{}, fmt.Errorf("short trun header")
	}
	flags := uint32(data[p+1])<<16 | uint32(data[p+2])<<8 | uint32(data[p+3])
	count := binary.BigEndian.Uint32(data[p+4 : p+8])
	q := p + 8
	var info trunInfo
	if flags&0x000001 != 0 {
		if q+4 > b.end {
			return info, fmt.Errorf("trun data offset truncated")
		}
		info.dataOffset = int32(binary.BigEndian.Uint32(data[q : q+4]))
		q += 4
	}
	if flags&0x000004 != 0 {
		q += 4
	}
	info.durations = make([]uint32, 0, count)
	info.sizes = make([]uint32, 0, count)
	for i := uint32(0); i < count; i++ {
		if flags&0x000100 != 0 {
			if q+4 > b.end {
				return info, fmt.Errorf("trun duration truncated")
			}
			info.durations = append(info.durations, binary.BigEndian.Uint32(data[q:q+4]))
			q += 4
		}
		if flags&0x000200 != 0 {
			if q+4 > b.end {
				return info, fmt.Errorf("trun size truncated")
			}
			info.sizes = append(info.sizes, binary.BigEndian.Uint32(data[q:q+4]))
			q += 4
		}
		if flags&0x000400 != 0 {
			q += 4
		}
		if flags&0x000800 != 0 {
			q += 4
		}
	}
	if len(info.sizes) != int(count) {
		return info, fmt.Errorf("trun sample sizes missing")
	}
	return info, nil
}

func auditError(msg string) *media.Error {
	return &media.Error{Code: media.CodeHashMismatch, Component: "audit.fmp4", Message: msg}
}
