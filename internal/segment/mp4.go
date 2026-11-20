// Package segment parses MP4 init segments and media segments box by box.
// Every box keeps its source offset, parsing never reads past the input
// buffer, and structural problems are reported as located faults: a
// truncated tail or an overrunning box is recoverable (earlier boxes stay
// usable), while a declared size smaller than the box header is not.
package segment

import (
	"bytes"
	"encoding/binary"

	"github.com/abema/go-mp4"

	"mediamap/internal/fault"
)

// Box is one parsed box with its absolute source offset.
type Box struct {
	Type   string `json:"type"`
	Offset int64  `json:"source_offset"`
	Size   int64  `json:"size"`
	Header int64  `json:"header"`
	Depth  int    `json:"depth"`
}

// Info is what the pipeline needs out of a segment.
type Info struct {
	Init       bool   `json:"init"`       // moov observed
	Keyframe   bool   `json:"keyframe"`   // styp declares msix
	DecodeTime int64  `json:"decode_time"` // tfdt base time, -1 when absent
	Sequence   uint32 `json:"sequence"`   // mfhd sequence, 0 when absent
	DataBytes  int64  `json:"data_bytes"` // mdat payload present
	Boxes      []Box  `json:"boxes"`
}

// MaxBoxes bounds the boxes parsed from one segment (resource cap).
const MaxBoxes = 4096

const maxNesting = 8

var containers = map[string]bool{
	"moov": true, "trak": true, "mdia": true, "minf": true, "stbl": true,
	"moof": true, "traf": true, "mvex": true, "edts": true, "dinf": true,
}

// Parse walks buf and returns the extracted info plus every fault found.
func Parse(track string, buf []byte) (Info, fault.List) {
	p := &parser{track: track, buf: buf}
	p.info.DecodeTime = -1
	p.walk(0, int64(len(buf)), 0)
	return p.info, p.faults
}

type parser struct {
	track  string
	buf    []byte
	info   Info
	faults fault.List
	fatal  bool
}

func (p *parser) walk(start, end int64, depth int) {
	if depth > maxNesting {
		p.faults = append(p.faults, fault.New(fault.StageSegment, p.track, start, false,
			"box nesting deeper than %d", maxNesting))
		p.fatal = true
		return
	}
	off := start
	for off < end && !p.fatal {
		if len(p.info.Boxes) >= MaxBoxes {
			p.faults = append(p.faults, fault.New(fault.StageSegment, p.track, off, false,
				"box count exceeds cap %d", MaxBoxes))
			p.fatal = true
			return
		}
		if end-off < 8 {
			p.faults = append(p.faults, fault.New(fault.StageSegment, p.track, off, true,
				"truncated box header: %d bytes remain", end-off))
			return
		}
		size := int64(binary.BigEndian.Uint32(p.buf[off : off+4]))
		typ := string(p.buf[off+4 : off+8])
		header := int64(8)
		switch size {
		case 1:
			if end-off < 16 {
				p.faults = append(p.faults, fault.New(fault.StageSegment, p.track, off, true,
					"truncated largesize header: %d bytes remain", end-off))
				return
			}
			size = int64(binary.BigEndian.Uint64(p.buf[off+8 : off+16]))
			header = 16
		case 0:
			size = end - off // box extends to end of input
		}
		if size < header {
			p.faults = append(p.faults, fault.New(fault.StageSegment, p.track, off, false,
				"box %q declares size %d below its %d-byte header", typ, size, header))
			p.fatal = true
			return
		}
		if off+size > end {
			if typ == "mdat" && end-off > header {
				p.info.DataBytes += end - off - header
			}
			p.faults = append(p.faults, fault.New(fault.StageSegment, p.track, off, true,
				"box %q declares %d bytes but only %d remain (truncated)", typ, size, end-off))
			return
		}
		p.record(Box{Type: typ, Offset: off, Size: size, Header: header, Depth: depth},
			p.buf[off+header:off+size])
		if containers[typ] {
			p.walk(off+header, off+size, depth+1)
		}
		off += size
	}
}

// record stores the box and extracts payload facts via go-mp4.
func (p *parser) record(b Box, payload []byte) {
	p.info.Boxes = append(p.info.Boxes, b)
	switch b.Type {
	case "moov":
		p.info.Init = true
	case "mdat":
		p.info.DataBytes += b.Size - b.Header
	case "styp":
		box, _, err := mp4.UnmarshalAny(bytes.NewReader(payload), mp4.BoxTypeStyp(),
			uint64(len(payload)), mp4.Context{})
		if err != nil {
			p.faults = append(p.faults, fault.New(fault.StageSegment, p.track, b.Offset, true,
				"styp payload unreadable: %v", err))
			return
		}
		styp := box.(*mp4.Styp)
		if string(styp.MajorBrand[:]) == "msix" {
			p.info.Keyframe = true
		}
		for _, br := range styp.CompatibleBrands {
			if string(br.CompatibleBrand[:]) == "msix" {
				p.info.Keyframe = true
			}
		}
	case "tfdt":
		box, _, err := mp4.UnmarshalAny(bytes.NewReader(payload), mp4.BoxTypeTfdt(),
			uint64(len(payload)), mp4.Context{})
		if err != nil {
			p.faults = append(p.faults, fault.New(fault.StageSegment, p.track, b.Offset, true,
				"tfdt payload unreadable: %v", err))
			return
		}
		p.info.DecodeTime = int64(box.(*mp4.Tfdt).GetBaseMediaDecodeTime())
	case "mfhd":
		box, _, err := mp4.UnmarshalAny(bytes.NewReader(payload), mp4.BoxTypeMfhd(),
			uint64(len(payload)), mp4.Context{})
		if err != nil {
			p.faults = append(p.faults, fault.New(fault.StageSegment, p.track, b.Offset, true,
				"mfhd payload unreadable: %v", err))
			return
		}
		p.info.Sequence = box.(*mp4.Mfhd).SequenceNumber
	}
}
