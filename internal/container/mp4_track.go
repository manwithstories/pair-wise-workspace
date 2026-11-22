// Track-level MP4 parsing: trak/mdia/minf/stbl, mdhd, hdlr, the audio
// sample entry, and the AAC AudioSpecificConfig inside esds.
package container

import (
	"fmt"

	"github.com/abema/go-mp4"

	"mediacore/internal/media"
)

type trackBuild struct {
	id           int
	trackOffset  int64
	timescale    int64
	duration     int64
	handler      string
	codecTag     string
	rate         int
	channels     int
	bits         int
	config       []byte
	sampleSizes  []uint32
	sampleSize   uint32
	stts         []mp4.SttsEntry
	ctts         []mp4.CttsEntry
	cttsVersion  uint8
	chunkOffsets []uint64
	stsc         []mp4.StscEntry
}

func (st *parseState) parseTrak(bi boxHeader) error {
	tb := &trackBuild{trackOffset: bi.start}
	err := children(st.src.r, bi.payload, bi.end, func(child boxHeader) error {
		switch child.typ {
		case "tkhd":
			return st.readTkhd(tb, child)
		case "mdia":
			return st.parseMdia(tb, child)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if tb.handler != "soun" {
		return nil
	}
	return st.finishTrack(tb, bi)
}

func (st *parseState) readTkhd(tb *trackBuild, bi boxHeader) error {
	var box mp4.Tkhd
	if err := st.decodeBox(bi, &box); err != nil {
		return err
	}
	tb.id = int(box.TrackID)
	return nil
}

func (st *parseState) parseMdia(tb *trackBuild, bi boxHeader) error {
	return children(st.src.r, bi.payload, bi.end, func(child boxHeader) error {
		switch child.typ {
		case "mdhd":
			return st.readMdhd(tb, child)
		case "hdlr":
			return st.readHdlr(tb, child)
		case "minf":
			return st.parseMinf(tb, child)
		}
		return nil
	})
}

func (st *parseState) readMdhd(tb *trackBuild, bi boxHeader) error {
	var box mp4.Mdhd
	if err := st.decodeBox(bi, &box); err != nil {
		return err
	}
	tb.timescale = int64(box.Timescale)
	tb.duration = int64(box.GetDuration())
	if tb.timescale <= 0 || tb.timescale > 1<<31-1 {
		return media.NewError(media.CodeUnsupportedTrack, bi.start, 0,
			"invalid mdhd timescale %d", box.Timescale)
	}
	return nil
}

func (st *parseState) readHdlr(tb *trackBuild, bi boxHeader) error {
	var box mp4.Hdlr
	if err := st.decodeBox(bi, &box); err != nil {
		return err
	}
	tb.handler = string(box.HandlerType[:])
	return nil
}

func (st *parseState) parseMinf(tb *trackBuild, bi boxHeader) error {
	return children(st.src.r, bi.payload, bi.end, func(child boxHeader) error {
		if child.typ == "stbl" {
			return st.parseStbl(tb, child)
		}
		return nil
	})
}

func (st *parseState) parseStbl(tb *trackBuild, bi boxHeader) error {
	return children(st.src.r, bi.payload, bi.end, func(child boxHeader) error {
		switch child.typ {
		case "stsd":
			return st.readStsd(tb, child)
		case "stts":
			return st.readStts(tb, child)
		case "ctts":
			return st.readCtts(tb, child)
		case "stsz":
			return st.readStsz(tb, child)
		case "stco":
			return st.readStco(tb, child, false)
		case "co64":
			return st.readStco(tb, child, true)
		case "stsc":
			return st.readStsc(tb, child)
		}
		return nil
	})
}

func (st *parseState) readStsd(tb *trackBuild, bi boxHeader) error {
	var box mp4.Stsd
	if err := st.decodeBox(bi, &box); err != nil {
		return err
	}
	// The first sample entry starts after version/flags + entry count.
	entryOff := bi.payload + 8
	if entryOff >= bi.end {
		return media.NewError(media.CodeInvalidBoxLength, bi.start, 0, "stsd has no sample entry")
	}
	entry, err := readBoxHeader(st.src.r, entryOff, bi.end)
	if err != nil {
		return err
	}
	tb.codecTag = entry.typ
	entBody, err := readPayload(st.src.r, entry)
	if err != nil {
		return err
	}
	if len(entBody) < 28 {
		return media.NewError(media.CodeInvalidBoxLength, entry.start, 0,
			"audio sample entry too short: %d", len(entBody))
	}
	// AudioSampleEntry after the 8-byte SampleEntry base:
	// reserved[6] data_reference_index(2) version(2) reserved[6]
	// channelcount(2) samplesize(2) pre_defined(2) reserved(2) rate(4).
	tb.channels = int(be16(entBody[16:18]))
	tb.bits = int(be16(entBody[18:20]))
	tb.rate = int(be32(entBody[24:28]) >> 16)
	if tb.rate == 0 {
		tb.rate = int(be32(entBody[24:28]))
	}
	return st.readEntryChildren(tb, entry, entBody)
}

// readEntryChildren walks the boxes (esds, wave, ...) below the sample entry.
func (st *parseState) readEntryChildren(tb *trackBuild, entry boxHeader, body []byte) error {
	bodyLen := int64(28)
	if len(body) >= 10 {
		switch be16(body[8:10]) {
		case 1:
			bodyLen = 44
		case 2:
			bodyLen = 64
		}
	}
	off := entry.payload + bodyLen
	if off > entry.end {
		off = entry.end
	}
	for off+8 <= entry.end {
		child, err := readBoxHeader(st.src.r, off, entry.end)
		if err != nil {
			return err
		}
		if child.typ == "esds" {
			payload, err := readPayload(st.src.r, child)
			if err != nil {
				return err
			}
			if cfg, err := extractASC(payload); err == nil && len(tb.config) == 0 {
				tb.config = cfg
			}
		}
		if child.end <= off {
			break
		}
		off = child.end
	}
	return nil
}

// extractASC scans an esds payload for the DecoderSpecificInfo descriptor
// (tag 0x05) and returns its bytes. Tolerance is deliberate: the codec layer
// validates the ASC and reports a precise error if it is malformed.
func extractASC(esds []byte) ([]byte, error) {
	i := 0
	if len(esds) >= 4 {
		i = 4 // version + flags
	}
	for i < len(esds) {
		tag := esds[i]
		i++
		size, n, ok := readDescLen(esds[i:])
		if !ok {
			return nil, fmt.Errorf("truncated descriptor length at %d", i)
		}
		i += n
		if size < 0 || i+size > len(esds) {
			return nil, fmt.Errorf("descriptor 0x%02x overruns esds", tag)
		}
		if tag == 0x05 {
			out := make([]byte, size)
			copy(out, esds[i:i+size])
			return out, nil
		}
		if tag == 0x03 || tag == 0x04 {
			inner := esds[i : i+size]
			if tag == 0x03 && len(inner) >= 3 {
				skip := 3
				flags := inner[2]
				if flags&0x80 != 0 {
					skip += 2
				}
				if flags&0x40 != 0 && skip < len(inner) {
					skip += 1 + int(inner[skip])
				}
				if flags&0x20 != 0 {
					skip += 2
				}
				if skip < len(inner) {
					inner = inner[skip:]
				} else {
					inner = nil
				}
			}
			if tag == 0x04 && len(inner) >= 13 {
				inner = inner[13:]
			}
			if cfg, err := extractASC(inner); err == nil && len(cfg) > 0 {
				return cfg, nil
			}
		}
		i += size
	}
	return nil, nil
}

func readDescLen(b []byte) (size int, n int, ok bool) {
	if len(b) == 0 {
		return 0, 0, false
	}
	v := 0
	for n = 0; n < 4 && n < len(b); n++ {
		v = v<<7 | int(b[n]&0x7f)
		if b[n]&0x80 == 0 {
			return v, n + 1, true
		}
	}
	return 0, 0, false
}
