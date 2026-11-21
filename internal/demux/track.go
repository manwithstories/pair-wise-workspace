package demux

import (
	"encoding/binary"

	"mediamerge/internal/model"
)

// supportedCodecs maps a sample entry type to its canonical codec name.
var supportedCodecs = map[string]string{
	"avc1": "h264", "avc3": "h264",
	"hvc1": "hevc", "hev1": "hevc",
	"mp4a": "aac", "alac": "alac",
}

type rawTrack struct {
	id         uint32
	handler    string
	timescale  uint32
	duration   uint64
	language   string
	width      uint16
	height     uint16
	channels   uint16
	sampleBits uint16
	sampleRate uint32
	entryType  string
	entry      []byte
	stts       []sttsRun
	ctts       []cttsRun
	stsc       []stscRun
	offsets    []uint64
	sizes      []uint32
	sync       []uint32
	stblBase   int64
	tableBoxes map[string]Table
	sizeBase   int64 // payload offset of the first stsz/stz2 entry
	sizeFixed  bool
	chunkBase  int64 // payload offset of the first stco/co64 entry
}

type sttsRun struct{ count, delta uint32 }
type cttsRun struct {
	count  uint32
	offset int32
}
type stscRun struct{ firstChunk, samplesPerChunk, descIndex uint32 }

func (f *File) parseTrak(box Table) (*Track, error) {
	children, err := splitBoxes(box.Payload, box.PayloadOffset())
	if err != nil {
		return nil, err
	}
	rt := &rawTrack{tableBoxes: map[string]Table{}}
	foundTkhd, foundMdhd, foundHdlr := false, false, false
	for _, c := range children {
		switch c.Type {
		case "tkhd":
			if len(c.Payload) < 4 {
				return nil, derr(model.StageDemux, model.CodeTruncated, c.PayloadOffset(), 0, false, "short tkhd")
			}
			rt.id = readTKHDTrackID(c.Payload)
			foundTkhd = true
		case "mdia":
			mdia, err := splitBoxes(c.Payload, c.PayloadOffset())
			if err != nil {
				return nil, err
			}
			for _, m := range mdia {
				switch m.Type {
				case "mdhd":
					ts, dur, lang, err := readMDHD(m)
					if err != nil {
						return nil, err
					}
					rt.timescale, rt.duration, rt.language = ts, dur, lang
					foundMdhd = true
				case "hdlr":
					h, err := readHDLR(m)
					if err != nil {
						return nil, err
					}
					rt.handler = h
					foundHdlr = true
				case "minf":
					if err := f.parseMinf(rt, m); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	if !foundTkhd || !foundMdhd || !foundHdlr {
		return nil, derr(model.StageDemux, model.CodeMissingBox, box.Offset, 0, false,
			"trak at %d missing tkhd/mdhd/hdlr", box.Offset)
	}
	if rt.timescale == 0 {
		return nil, derr(model.StageDemux, model.CodeTimebase, box.Offset, rt.id, false,
			"track %d has zero timescale", rt.id)
	}
	if err := f.finishTrack(rt); err != nil {
		return nil, err
	}
	return f.toTrack(rt), nil
}

func (f *File) parseMinf(rt *rawTrack, minf Table) error {
	children, err := splitBoxes(minf.Payload, minf.PayloadOffset())
	if err != nil {
		return err
	}
	for _, c := range children {
		if c.Type != "stbl" {
			continue
		}
		stbl, err := splitBoxes(c.Payload, c.PayloadOffset())
		if err != nil {
			return err
		}
		rt.stblBase = c.PayloadOffset()
		for _, b := range stbl {
			rt.tableBoxes[b.Type] = b
			switch b.Type {
			case "stsd":
				if err := parseSTSD(rt, b); err != nil {
					return err
				}
			case "stts":
				rt.stts, err = parseSTTS(b)
			case "ctts":
				rt.ctts, err = parseCTTS(b)
			case "stsc":
				rt.stsc, err = parseSTSC(b)
			case "stsz":
				rt.sizes, err = parseSTSZ(b)
				rt.sizeBase = b.PayloadOffset() + 12
				rt.sizeFixed = len(b.Payload) >= 12 && binary.BigEndian.Uint32(b.Payload[4:8]) != 0
				if rt.sizeFixed {
					rt.sizeBase = b.PayloadOffset() + 4
				}
			case "stz2":
				rt.sizes, err = parseSTZ2(b)
				rt.sizeBase = b.PayloadOffset() + 12
				if len(b.Payload) > 7 && b.Payload[7] == 16 {
					rt.sizeBase = b.PayloadOffset() + 12
				}
			case "stco":
				rt.offsets, err = parseSTCO(b)
				rt.chunkBase = b.PayloadOffset() + 8
			case "co64":
				rt.offsets, err = parseCO64(b)
				rt.chunkBase = b.PayloadOffset() + 8
			case "stss":
				rt.sync, err = parseSTSS(b)
			case "sdtp", "sbgp", "sgpd", "padb", "stsh", "subs", "saiz", "saio", "senc":
				// tolerated auxiliary sample table boxes; they are not needed
				// for lossless remuxing since samples are copied verbatim.
			default:
				return derr(model.StageDemux, model.CodeUnknownBox, b.Offset, rt.id, false,
					"unknown required sample table box %q", b.Type)
			}
			if err != nil {
				return err
			}
		}
	}
	if _, ok := rt.tableBoxes["stsd"]; !ok {
		return derr(model.StageDemux, model.CodeMissingBox, minf.Offset, rt.id, false, "missing stsd box")
	}
	if _, ok := rt.tableBoxes["stts"]; !ok {
		return derr(model.StageDemux, model.CodeMissingBox, minf.Offset, rt.id, false, "missing stts box")
	}
	if _, ok := rt.tableBoxes["stsc"]; !ok {
		return derr(model.StageDemux, model.CodeMissingBox, minf.Offset, rt.id, false, "missing stsc box")
	}
	if _, ok := rt.tableBoxes["stsz"]; !ok {
		if _, ok := rt.tableBoxes["stz2"]; !ok {
			return derr(model.StageDemux, model.CodeMissingBox, minf.Offset, rt.id, false, "missing stsz box")
		}
	}
	return nil
}

func readTKHDTrackID(p []byte) uint32 {
	ver := p[0]
	if ver == 1 {
		if len(p) >= 24 {
			return binary.BigEndian.Uint32(p[20:24])
		}
		return 0
	}
	if len(p) >= 16 {
		return binary.BigEndian.Uint32(p[12:16])
	}
	return 0
}

func readMDHD(b Table) (uint32, uint64, string, error) {
	p := b.Payload
	if len(p) < 4 {
		return 0, 0, "", derr(model.StageDemux, model.CodeTruncated, b.PayloadOffset(), 0, false, "short mdhd")
	}
	if p[0] == 1 {
		if len(p) < 36 {
			return 0, 0, "", derr(model.StageDemux, model.CodeTruncated, b.PayloadOffset(), 0, false, "short v1 mdhd")
		}
		return binary.BigEndian.Uint32(p[20:24]), binary.BigEndian.Uint64(p[24:32]),
			decodeLang(p[32:34]), nil
	}
	if len(p) < 24 {
		return 0, 0, "", derr(model.StageDemux, model.CodeTruncated, b.PayloadOffset(), 0, false, "short v0 mdhd")
	}
	return binary.BigEndian.Uint32(p[12:16]), uint64(binary.BigEndian.Uint32(p[16:20])),
		decodeLang(p[20:22]), nil
}

func decodeLang(p []byte) string {
	if len(p) < 2 {
		return "und"
	}
	v := binary.BigEndian.Uint16(p)
	out := []byte{
		byte((v>>10)&0x1F) + 0x60,
		byte((v>>5)&0x1F) + 0x60,
		byte(v&0x1F) + 0x60,
	}
	for _, c := range out {
		if c < 'a' || c > 'z' {
			return "und"
		}
	}
	return string(out)
}

func readHDLR(b Table) (string, error) {
	p := b.Payload
	if len(p) < 12 {
		return "", derr(model.StageDemux, model.CodeTruncated, b.PayloadOffset(), 0, false, "short hdlr")
	}
	return string(p[8:12]), nil
}

func parseSTSD(rt *rawTrack, b Table) error {
	p := b.Payload
	if len(p) < 8 {
		return derr(model.StageDemux, model.CodeTruncated, b.PayloadOffset(), rt.id, false, "short stsd")
	}
	count := binary.BigEndian.Uint32(p[4:8])
	if count == 0 {
		return derr(model.StageDemux, model.CodeMissingBox, b.Offset, rt.id, false, "stsd has no entries")
	}
	start := 8
	if len(p)-start < 8 {
		return derr(model.StageDemux, model.CodeSampleTable, b.PayloadOffset()+int64(start), rt.id, false,
			"stsd entry table truncated")
	}
	size := int(binary.BigEndian.Uint32(p[start : start+4]))
	typ := string(p[start+4 : start+8])
	if size < 8 || start+size > len(p) {
		return derr(model.StageDemux, model.CodeSampleTable, b.PayloadOffset()+int64(start), rt.id, false,
			"stsd entry %q has invalid size %d", typ, size)
	}
	rt.entryType = typ
	rt.entry = append([]byte(nil), p[start:start+size]...)
	if rt.handler == "vide" || typ == "avc1" || typ == "hvc1" || typ == "avc3" || typ == "hev1" {
		if size >= 8+8+70 && size >= 86 {
			rt.width = binary.BigEndian.Uint16(p[start+32 : start+34])
			rt.height = binary.BigEndian.Uint16(p[start+34 : start+36])
		}
	}
	if rt.handler == "soun" {
		if size >= 8+8+20 {
			rt.channels = binary.BigEndian.Uint16(p[start+24 : start+26])
			rt.sampleBits = binary.BigEndian.Uint16(p[start+26 : start+28])
			rt.sampleRate = binary.BigEndian.Uint32(p[start+32:start+36]) >> 16
		}
	}
	return nil
}
