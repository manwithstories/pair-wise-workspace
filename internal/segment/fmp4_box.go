package segment

import (
	"encoding/binary"
	"fmt"

	"aircheck/internal/media"
)

// CMAF/fMP4 box types aircheck understands.
const (
	boxFTYP = "ftyp"
	boxMOOV = "moov"
	boxMOOF = "moof"
	boxMDAT = "mdat"
	boxTRAK = "trak"
	boxMDIA = "mdia"
	boxMINF = "minf"
	boxSTBL = "stbl"
	boxSTSD = "stsd"
	boxSTTS = "stts"
	boxSTSZ = "stsz"
	boxSTSC = "stsc"
	boxSTCO = "stco"
	boxCO64 = "co64"
	boxMVHD = "mvhd"
	boxMDHD = "mdhd"
	boxHDLR = "hdlr"
	boxTFHD = "tfhd"
	boxTFDT = "tfdt"
	boxTRUN = "trun"
	boxTREX = "trex"
	boxELST = "elst"
)

// FMP4Options configures CMAF demuxing.
type FMP4Options struct {
	// Init supplies the initialisation segment (ftyp+moov). A media segment
	// that arrives without one fails with FaultMissingInit.
	Init []byte
	// InitName is the artifact name of the init section for fault reporting.
	InitName string
}

// Box is a parsed ISO-BMFF box header with byte extents in its parent file.
type Box struct {
	Type   string
	Offset int64 // absolute offset of the box header
	Header int64 // header length (8 or 16)
	Size   int64 // full box size including header
}

// End returns the byte offset one past the box.
func (b Box) End() int64 { return b.Offset + b.Size }

// Payload returns the slice of data covered by the box body.
func (b Box) Payload(data []byte, origin int64) []byte {
	start := b.Offset - origin + b.Header
	end := b.Offset - origin + b.Size
	if start < 0 || end > int64(len(data)) || start >= end {
		return nil
	}
	return data[start:end]
}

// ReadBoxes iterates siblings at data[origin:origin+limit] (limit 0 = rest of
// data) and returns them with absolute offsets. A box that overruns its parent,
// has size < 8, or claims a 64-bit size beyond the region is a FaultBadBoxLength.
func ReadBoxes(artifact string, data []byte, origin, limit int64) ([]Box, error) {
	var out []Box
	pos := origin
	end := int64(len(data))
	if limit > 0 && origin+limit < end {
		end = origin + limit
	}
	for pos < end {
		if end-pos < 8 {
			return nil, media.NewFault(media.FaultTruncated,
				media.SourcePos{Artifact: artifact, Offset: pos},
				fmt.Sprintf("only %d bytes left for a box header", end-pos))
		}
		size := int64(binary.BigEndian.Uint32(data[pos : pos+4]))
		typ := string(data[pos+4 : pos+8])
		header := int64(8)
		if size == 1 {
			if end-pos < 16 {
				return nil, media.NewFault(media.FaultTruncated,
					media.SourcePos{Artifact: artifact, Offset: pos}, "truncated largesize header")
			}
			l64 := binary.BigEndian.Uint64(data[pos+8 : pos+16])
			if l64 > uint64(end-pos) {
				return nil, media.NewFault(media.FaultBadBoxLength,
					media.SourcePos{Artifact: artifact, Offset: pos},
					fmt.Sprintf("box %q largesize %d exceeds %d available bytes", typ, l64, end-pos))
			}
			size = int64(l64)
			header = 16
		} else if size == 0 {
			// size 0 means "to end of file" only on the last top-level box.
			size = end - pos
		}
		if size < header {
			return nil, media.NewFault(media.FaultBadBoxLength,
				media.SourcePos{Artifact: artifact, Offset: pos},
				fmt.Sprintf("box %q size %d smaller than header %d", typ, size, header))
		}
		if pos+size > end {
			return nil, media.NewFault(media.FaultBadBoxLength,
				media.SourcePos{Artifact: artifact, Offset: pos},
				fmt.Sprintf("box %q size %d overruns region by %d", typ, size, pos+size-end))
		}
		if typ == boxMOOF && size < header+8 {
			return nil, media.NewFault(media.FaultBadBoxLength,
				media.SourcePos{Artifact: artifact, Offset: pos}, "moof smaller than its child header")
		}
		out = append(out, Box{Type: typ, Offset: pos, Header: header, Size: size})
		pos += size
	}
	if pos != end {
		return nil, media.NewFault(media.FaultTruncated,
			media.SourcePos{Artifact: artifact, Offset: pos}, "box parsing left trailing bytes")
	}
	return out, nil
}

// find returns the first child of the given type.
func find(boxes []Box, typ string) (Box, bool) {
	for _, b := range boxes {
		if b.Type == typ {
			return b, true
		}
	}
	return Box{}, false
}

// childBoxes reads the children of a container box.
func childBoxes(artifact string, data []byte, parent Box, origin int64) ([]Box, error) {
	return ReadBoxes(artifact, data, parent.Offset+parent.Header, parent.Size-parent.Header)
}

// fullBox returns the version, 24-bit flags and the complete payload of a full
// box. The payload includes the 4-byte version/flags prefix so callers index
// fields exactly as the specification orders them.
func fullBox(artifact string, data []byte, b Box, origin int64) (byte, uint32, []byte, error) {
	p := b.Payload(data, origin)
	if len(p) < 4 {
		return 0, 0, nil, media.NewFault(media.FaultTruncated,
			media.SourcePos{Artifact: artifact, Offset: b.Offset}, b.Type+" shorter than version/flags")
	}
	flags := uint32(p[1])<<16 | uint32(p[2])<<8 | uint32(p[3])
	return p[0], flags, p, nil
}

// trackInfo is parsed from a trak's mdhd/hdlr/stsd.
type trackInfo struct {
	id            uint32
	kind          media.Kind
	codec         string
	timescale     int64
	duration      int64
	sampleRate    int
	channels      int
	width, height int
	init          []byte // the trak box bytes, re-emitted verbatim by the remuxer
	defaultDur    int64
}

// parseMoov extracts the track description from an init segment.
func parseMoov(artifact string, data []byte, moov Box, origin int64) ([]trackInfo, error) {
	traks, err := childBoxes(artifact, data, moov, origin)
	if err != nil {
		return nil, err
	}
	var out []trackInfo
	for _, tr := range traks {
		if tr.Type != boxTRAK {
			continue
		}
		ti, err := parseTrak(artifact, data, tr, origin)
		if err != nil {
			return nil, err
		}
		out = append(out, ti)
	}
	return out, nil
}

func parseTrak(artifact string, data []byte, trak Box, origin int64) (trackInfo, error) {
	ti := trackInfo{init: data[trak.Offset-origin : trak.Offset-origin+trak.Size]}
	kids, err := childBoxes(artifact, data, trak, origin)
	if err != nil {
		return ti, err
	}
	if mdia, ok := find(kids, boxMDIA); ok {
		mkids, err := childBoxes(artifact, data, mdia, origin)
		if err != nil {
			return ti, err
		}
		if mdhd, ok := find(mkids, boxMDHD); ok {
			ver, _, body, err := fullBox(artifact, data, mdhd, origin)
			if err != nil {
				return ti, err
			}
			// mdhd: creation, modification, timescale, duration.
			if ver == 1 {
				if len(body) < 32 {
					return ti, media.NewFault(media.FaultTruncated,
						media.SourcePos{Artifact: artifact, Offset: mdhd.Offset}, "mdhd v1 short")
				}
				ti.timescale = int64(binary.BigEndian.Uint32(body[20:24]))
				ti.duration = int64(binary.BigEndian.Uint64(body[24:32]))
			} else {
				if len(body) < 20 {
					return ti, media.NewFault(media.FaultTruncated,
						media.SourcePos{Artifact: artifact, Offset: mdhd.Offset}, "mdhd v0 short")
				}
				ti.timescale = int64(binary.BigEndian.Uint32(body[12:16]))
				ti.duration = int64(binary.BigEndian.Uint32(body[16:20]))
			}
			if ti.timescale <= 0 {
				return ti, media.NewFault(media.FaultBadBoxLength,
					media.SourcePos{Artifact: artifact, Offset: mdhd.Offset}, "mdhd timescale 0")
			}
		}
		if hdlr, ok := find(mkids, boxHDLR); ok {
			_, _, body, err := fullBox(artifact, data, hdlr, origin)
			if err != nil {
				return ti, err
			}
			// hdlr: pre_defined(4), handler_type(4).
			if len(body) >= 12 {
				switch string(body[8:12]) {
				case "vide":
					ti.kind = media.KindVideo
				case "soun":
					ti.kind = media.KindAudio
				}
			}
		}
		if minf, ok := find(mkids, boxMINF); ok {
			if mk, err := childBoxes(artifact, data, minf, origin); err == nil {
				if stbl, ok := find(mk, boxSTBL); ok {
					if sk, err := childBoxes(artifact, data, stbl, origin); err == nil {
						if stsd, ok := find(sk, boxSTSD); ok {
							codec, sr, ch, w, h, err := parseSTSD(artifact, data, stsd, origin)
							if err != nil {
								return ti, err
							}
							ti.codec, ti.sampleRate, ti.channels, ti.width, ti.height = codec, sr, ch, w, h
						}
					}
				}
			}
		}
	}
	if tkhd, ok := find(kids, "tkhd"); ok {
		ver, _, body, err := fullBox(artifact, data, tkhd, origin)
		if err == nil {
			// tkhd: version/flags(4), creation, modification, track_ID.
			// v0 uses 32-bit times; v1 widens them to 64-bit.
			idx := 12
			if ver == 1 {
				idx = 20
			}
			if len(body) >= idx+4 {
				ti.id = binary.BigEndian.Uint32(body[idx : idx+4])
			}
		}
	}

	if ti.kind == 0 {
		return ti, fmt.Errorf("trak %d has no video or audio handler", ti.id)
	}
	return ti, nil
}

// parseSTSD reads the sample entry codec fourcc and audio parameters.
func parseSTSD(artifact string, data []byte, stsd Box, origin int64) (codec string, sampleRate, channels, width, height int, err error) {
	_, _, body, err := fullBox(artifact, data, stsd, origin)
	if err != nil {
		return "", 0, 0, 0, 0, err
	}
	if len(body) < 8 {
		return "", 0, 0, 0, 0, media.NewFault(media.FaultTruncated,
			media.SourcePos{Artifact: artifact, Offset: stsd.Offset}, "stsd entry count missing")
	}
	count := binary.BigEndian.Uint32(body[4:8])
	if count == 0 || len(body) < 16 {
		return "", 0, 0, 0, 0, media.NewFault(media.FaultBadBoxLength,
			media.SourcePos{Artifact: artifact, Offset: stsd.Offset}, "stsd lists no sample entry")
	}
	entrySize := int64(binary.BigEndian.Uint32(body[8:12]))
	entryType := string(body[12:16])
	entry := body[8:]
	if int64(len(entry)) < entrySize || entrySize < 16 {
		return "", 0, 0, 0, 0, media.NewFault(media.FaultBadBoxLength,
			media.SourcePos{Artifact: artifact, Offset: stsd.Offset},
			fmt.Sprintf("sample entry size %d invalid", entrySize))
	}
	entry = entry[:entrySize]
	switch entryType {
	case "avc1", "avc3", "hvc1", "hev1":
		if len(entry) >= 36 {
			width = int(binary.BigEndian.Uint16(entry[32:34]))
			height = int(binary.BigEndian.Uint16(entry[34:36]))
		}
		return entryType, 0, 0, width, height, nil
	case "mp4a", "ac-3", "ec-3", "Opus":
		// AudioSampleEntry: 8 reserved, 2 channelcount, 2 samplesize,
		// 4 pre_defined/reserved, 2 samplerate (16.16 fixed).
		if len(entry) >= 36 {
			channels = int(binary.BigEndian.Uint16(entry[24:26]))
			sampleRate = int(binary.BigEndian.Uint32(entry[32:36]) >> 16)
		}
		return entryType, sampleRate, channels, 0, 0, nil
	}
	return entryType, 0, 0, 0, 0, nil
}

// trexDefaults holds per-track defaults from the init section.
type trexDefaults struct {
	trackID           uint32
	defaultSampleDesc uint32
	defaultDur        int64
	defaultSize       int64
	defaultFlags      uint32
}

// parseTrex reads trex boxes from the moov.
func parseTrex(artifact string, data []byte, moov Box, origin int64) (map[uint32]trexDefaults, error) {
	kids, err := childBoxes(artifact, data, moov, origin)
	if err != nil {
		return nil, err
	}
	out := map[uint32]trexDefaults{}
	for _, k := range kids {
		if k.Type != boxTREX {
			continue
		}
		_, _, body, err := fullBox(artifact, data, k, origin)
		if err != nil {
			return nil, err
		}
		// trex: track_ID, default_sample_description_index,
		// default_sample_duration, default_sample_size, default_sample_flags.
		if len(body) < 24 {
			return nil, media.NewFault(media.FaultTruncated,
				media.SourcePos{Artifact: artifact, Offset: k.Offset}, "trex short")
		}
		d := trexDefaults{
			trackID:           binary.BigEndian.Uint32(body[4:8]),
			defaultSampleDesc: binary.BigEndian.Uint32(body[8:12]),
			defaultDur:        int64(binary.BigEndian.Uint32(body[12:16])),
			defaultSize:       int64(binary.BigEndian.Uint32(body[16:20])),
			defaultFlags:      binary.BigEndian.Uint32(body[20:24]),
		}
		out[d.trackID] = d
	}
	return out, nil
}
