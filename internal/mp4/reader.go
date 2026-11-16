package mp4

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
)

type ParseError struct {
	Message string
}

func (e *ParseError) Error() string { return e.Message }

func parseError(format string, args ...any) error {
	return &ParseError{Message: fmt.Sprintf(format, args...)}
}

type InitInfo struct {
	Timescale uint32
	Handler   string
	Width     uint32
	Height    uint32
}

type SegmentInfo struct {
	TrackID     uint32
	StartTime   int64
	Duration    int64
	Timescale   uint32
	SampleCount uint32
	ByteStart   int64
	ByteEnd     int64
	HasTFDT     bool
}

type box struct {
	Type         string
	Start        int64
	Size         int64
	HeaderSize   int64
	PayloadStart int64
	PayloadEnd   int64
	Children     []box
}

var containers = map[string]bool{
	"moov": true, "trak": true, "mdia": true, "minf": true,
	"stbl": true, "mvex": true, "moof": true, "traf": true,
	"mfra": true, "dinf": true,
}

func ParseInitFile(path string) (InitInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return InitInfo{}, err
	}
	return ParseInit(data)
}

func ParseInit(data []byte) (InitInfo, error) {
	boxes, err := parseBoxes(data, 0, int64(len(data)), 0)
	if err != nil {
		return InitInfo{}, err
	}
	moov := findFirst(boxes, "moov")
	if moov == nil {
		return InitInfo{}, parseError("init has no moov box")
	}
	mdhd := findFirst(moov.Children, "mdhd")
	if mdhd == nil {
		return InitInfo{}, parseError("init has no mdhd box")
	}
	info := InitInfo{}
	info.Timescale, err = parseMDHD(data, *mdhd)
	if err != nil {
		return InitInfo{}, err
	}
	if hdlr := findFirst(moov.Children, "hdlr"); hdlr != nil {
		if hdlr.PayloadStart+12 > hdlr.PayloadEnd {
			return InitInfo{}, parseError("truncated hdlr box")
		}
		info.Handler = string(data[hdlr.PayloadStart+8 : hdlr.PayloadStart+12])
	}
	if tkhd := findFirst(moov.Children, "tkhd"); tkhd != nil &&
		tkhd.PayloadEnd-tkhd.PayloadStart >= 8 {
		info.Width = binary.BigEndian.Uint32(data[tkhd.PayloadEnd-8:]) >> 16
		info.Height = binary.BigEndian.Uint32(data[tkhd.PayloadEnd-4:]) >> 16
	}
	return info, nil
}

func parseMDHD(data []byte, b box) (uint32, error) {
	payload := data[b.PayloadStart:b.PayloadEnd]
	if len(payload) < 4 {
		return 0, parseError("truncated mdhd box")
	}
	var offset int
	switch payload[0] {
	case 0:
		offset = 12
	case 1:
		offset = 20
	default:
		return 0, parseError("unsupported mdhd version %d", payload[0])
	}
	if offset+4 > len(payload) {
		return 0, parseError("truncated mdhd timescale")
	}
	timescale := binary.BigEndian.Uint32(payload[offset:])
	if timescale == 0 {
		return 0, parseError("mdhd timescale is zero")
	}
	return timescale, nil
}

func parseBoxes(data []byte, start, end int64, depth int) ([]box, error) {
	if depth > 16 {
		return nil, parseError("box nesting is too deep")
	}
	if start < 0 || end < start || end > int64(len(data)) {
		return nil, parseError("invalid box bounds")
	}
	var result []box
	for offset := start; offset < end; {
		if end-offset < 8 {
			return nil, parseError("truncated box header at offset %d", offset)
		}
		size := int64(binary.BigEndian.Uint32(data[offset:]))
		typ := string(data[offset+4 : offset+8])
		headerSize := int64(8)
		if size == 1 {
			if end-offset < 16 {
				return nil, parseError("truncated 64-bit box header at offset %d", offset)
			}
			extended := binary.BigEndian.Uint64(data[offset+8:])
			if extended > math.MaxInt64 {
				return nil, parseError("box %q is too large", typ)
			}
			size, headerSize = int64(extended), 16
		} else if size == 0 {
			size = end - offset
		}
		if size < headerSize || size > end-offset {
			return nil, parseError("invalid size %d for box %q at offset %d", size, typ, offset)
		}
		current := box{
			Type: typ, Start: offset, Size: size, HeaderSize: headerSize,
			PayloadStart: offset + headerSize, PayloadEnd: offset + size,
		}
		if containers[typ] {
			children, err := parseBoxes(data, current.PayloadStart, current.PayloadEnd, depth+1)
			if err != nil {
				return nil, err
			}
			current.Children = children
		}
		result = append(result, current)
		offset += size
	}
	return result, nil
}

func findFirst(boxes []box, typ string) *box {
	for i := range boxes {
		if boxes[i].Type == typ {
			return &boxes[i]
		}
		if found := findFirst(boxes[i].Children, typ); found != nil {
			return found
		}
	}
	return nil
}

func findAll(boxes []box, typ string, result *[]box) {
	for i := range boxes {
		if boxes[i].Type == typ {
			*result = append(*result, boxes[i])
		}
		findAll(boxes[i].Children, typ, result)
	}
}

type sidxReference struct {
	Duration int64
	Size     int64
}

type sidxInfo struct {
	Timescale   uint32
	Earliest    int64
	FirstOffset int64
	BoxEnd      int64
	References  []sidxReference
}

func ReadSegmentFile(path string, fallbackTimescale uint32) ([]SegmentInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ReadSegment(data, fallbackTimescale)
}

func ReadSegment(data []byte, fallbackTimescale uint32) ([]SegmentInfo, error) {
	boxes, err := parseBoxes(data, 0, int64(len(data)), 0)
	if err != nil {
		return nil, err
	}
	var index *sidxInfo
	if sidxBox := findFirst(boxes, "sidx"); sidxBox != nil {
		index, err = parseSidx(data, *sidxBox)
		if err != nil {
			return nil, err
		}
	}
	if index != nil && index.Timescale != 0 {
		fallbackTimescale = index.Timescale
	}
	if fallbackTimescale == 0 {
		return nil, parseError("segment has no sidx and no fallback timescale")
	}
	var moofs []box
	findAll(boxes, "moof", &moofs)
	if len(moofs) == 0 {
		if index == nil {
			return nil, parseError("segment contains neither moof nor sidx")
		}
		return segmentsFromSidx(index)
	}
	result := make([]SegmentInfo, 0, len(moofs))
	fallbackStart := int64(0)
	if index != nil {
		fallbackStart = index.Earliest
	}
	for i, moof := range moofs {
		mdat := nextMDAT(boxes, moof.Start)
		if mdat == nil {
			return nil, parseError("moof at offset %d has no following mdat", moof.Start)
		}
		var trafs []box
		for _, child := range moof.Children {
			if child.Type == "traf" {
				trafs = append(trafs, child)
			}
		}
		if len(trafs) == 0 {
			return nil, parseError("moof at offset %d has no traf", moof.Start)
		}
		for _, traf := range trafs {
			info, err := parseTraf(data, traf)
			if err != nil {
				return nil, err
			}
			info.Timescale = fallbackTimescale
			info.ByteStart = moof.Start
			info.ByteEnd = mdat.Start + mdat.Size
			if !info.HasTFDT {
				info.StartTime = fallbackStart
			}
			if index != nil && i < len(index.References) {
				if info.Duration <= 0 {
					info.Duration = index.References[i].Duration
				}
				fallbackStart += index.References[i].Duration
			} else if info.Duration > 0 {
				fallbackStart += info.Duration
			}
			if info.Duration <= 0 {
				return nil, parseError("fragment %d has no positive duration", i)
			}
			result = append(result, info)
		}
	}
	return result, nil
}

func nextMDAT(boxes []box, after int64) *box {
	for i := range boxes {
		if boxes[i].Type == "mdat" && boxes[i].Start > after {
			return &boxes[i]
		}
	}
	return nil
}

func parseSidx(data []byte, b box) (*sidxInfo, error) {
	p := b.PayloadStart
	end := b.PayloadEnd
	if p+12 > end {
		return nil, parseError("truncated sidx box")
	}
	version := data[p]
	info := &sidxInfo{Timescale: binary.BigEndian.Uint32(data[p+8:]), BoxEnd: b.Start + b.Size}
	var countOffset int64
	switch version {
	case 0:
		if p+24 > end {
			return nil, parseError("truncated version 0 sidx")
		}
		info.Earliest = int64(binary.BigEndian.Uint32(data[p+12:]))
		info.FirstOffset = int64(binary.BigEndian.Uint32(data[p+16:]))
		countOffset = p + 22
	case 1:
		if p+32 > end {
			return nil, parseError("truncated version 1 sidx")
		}
		earliest := binary.BigEndian.Uint64(data[p+12:])
		first := binary.BigEndian.Uint64(data[p+20:])
		if earliest > math.MaxInt64 || first > math.MaxInt64 {
			return nil, parseError("sidx timestamp overflows int64")
		}
		info.Earliest, info.FirstOffset = int64(earliest), int64(first)
		countOffset = p + 30
	default:
		return nil, parseError("unsupported sidx version %d", version)
	}
	count := int(binary.BigEndian.Uint16(data[countOffset:]))
	offset := countOffset + 2
	if offset+int64(count*12) > end {
		return nil, parseError("truncated sidx references")
	}
	for i := 0; i < count; i++ {
		rawSize := binary.BigEndian.Uint32(data[offset:])
		duration := binary.BigEndian.Uint32(data[offset+4:])
		info.References = append(info.References, sidxReference{
			Size: int64(rawSize & 0x7fffffff), Duration: int64(duration),
		})
		offset += 12
	}
	return info, nil
}

func segmentsFromSidx(index *sidxInfo) ([]SegmentInfo, error) {
	result := make([]SegmentInfo, 0, len(index.References))
	start := index.Earliest
	byteStart := index.BoxEnd + index.FirstOffset
	for i, reference := range index.References {
		if reference.Duration <= 0 || reference.Size <= 0 {
			return nil, parseError("sidx reference %d is empty", i)
		}
		result = append(result, SegmentInfo{
			StartTime: start, Duration: reference.Duration,
			Timescale: index.Timescale, ByteStart: byteStart,
			ByteEnd: byteStart + reference.Size,
		})
		start += reference.Duration
		byteStart += reference.Size
	}
	return result, nil
}

type tfhdInfo struct {
	TrackID         uint32
	DefaultDuration int64
}

func parseTraf(data []byte, traf box) (SegmentInfo, error) {
	var tfhdBox, tfdtBox *box
	var truns []box
	for i := range traf.Children {
		child := &traf.Children[i]
		switch child.Type {
		case "tfhd":
			tfhdBox = child
		case "tfdt":
			tfdtBox = child
		case "trun":
			truns = append(truns, *child)
		}
	}
	if tfhdBox == nil {
		return SegmentInfo{}, parseError("traf has no tfhd")
	}
	tfhd, err := parseTFHD(data, *tfhdBox)
	if err != nil {
		return SegmentInfo{}, err
	}
	info := SegmentInfo{TrackID: tfhd.TrackID}
	if tfdtBox != nil {
		info.StartTime, err = parseTFDT(data, *tfdtBox)
		if err != nil {
			return SegmentInfo{}, err
		}
		info.HasTFDT = true
	}
	for _, trun := range truns {
		samples, duration, err := parseTRUN(data, trun, tfhd.DefaultDuration)
		if err != nil {
			return SegmentInfo{}, err
		}
		info.SampleCount += samples
		info.Duration += duration
	}
	return info, nil
}

func parseTFHD(data []byte, b box) (tfhdInfo, error) {
	p, end := b.PayloadStart, b.PayloadEnd
	if p+8 > end {
		return tfhdInfo{}, parseError("truncated tfhd box")
	}
	flags := binary.BigEndian.Uint32(data[p:]) & 0x00ffffff
	info := tfhdInfo{TrackID: binary.BigEndian.Uint32(data[p+4:])}
	offset := p + 8
	if flags&0x000001 != 0 {
		offset += 8
	}
	if flags&0x000002 != 0 {
		offset += 4
	}
	if flags&0x000008 != 0 {
		if offset+4 > end {
			return tfhdInfo{}, parseError("truncated tfhd default duration")
		}
		info.DefaultDuration = int64(binary.BigEndian.Uint32(data[offset:]))
		offset += 4
	}
	if flags&0x000010 != 0 {
		offset += 4
	}
	if flags&0x000020 != 0 {
		offset += 4
	}
	if offset > end {
		return tfhdInfo{}, parseError("truncated tfhd optional fields")
	}
	return info, nil
}

func parseTFDT(data []byte, b box) (int64, error) {
	p, end := b.PayloadStart, b.PayloadEnd
	if p+4 > end {
		return 0, parseError("truncated tfdt box")
	}
	switch data[p] {
	case 0:
		if p+8 > end {
			return 0, parseError("truncated version 0 tfdt")
		}
		return int64(binary.BigEndian.Uint32(data[p+4:])), nil
	case 1:
		if p+12 > end {
			return 0, parseError("truncated version 1 tfdt")
		}
		value := binary.BigEndian.Uint64(data[p+4:])
		if value > math.MaxInt64 {
			return 0, parseError("tfdt timestamp overflows int64")
		}
		return int64(value), nil
	default:
		return 0, parseError("unsupported tfdt version %d", data[p])
	}
}

func parseTRUN(data []byte, b box, defaultDuration int64) (uint32, int64, error) {
	p, end := b.PayloadStart, b.PayloadEnd
	if p+8 > end {
		return 0, 0, parseError("truncated trun box")
	}
	flags := binary.BigEndian.Uint32(data[p:]) & 0x00ffffff
	count := binary.BigEndian.Uint32(data[p+4:])
	if count > 10_000_000 {
		return 0, 0, parseError("trun sample count is unreasonable")
	}
	offset := p + 8
	if flags&0x000001 != 0 {
		offset += 4
	}
	if flags&0x000004 != 0 {
		offset += 4
	}
	var duration int64
	for i := uint32(0); i < count; i++ {
		sampleDuration := defaultDuration
		if flags&0x000100 != 0 {
			if offset+4 > end {
				return 0, 0, parseError("truncated trun sample duration")
			}
			sampleDuration = int64(binary.BigEndian.Uint32(data[offset:]))
			offset += 4
		}
		if flags&0x000200 != 0 {
			offset += 4
		}
		if flags&0x000400 != 0 {
			offset += 4
		}
		if flags&0x000800 != 0 {
			offset += 4
		}
		if offset > end {
			return 0, 0, parseError("truncated trun sample fields")
		}
		duration += sampleDuration
	}
	return count, duration, nil
}
