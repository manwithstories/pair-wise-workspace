package mp4

import (
	"bytes"

	gomp4 "github.com/abema/go-mp4"

	"mediabridge/internal/fault"
	"mediabridge/internal/media"
)

// collectTrakBoxes walks the children of a trak box, capturing the boxes the
// reader needs. Unknown boxes are skipped; a malformed box that we do need is
// reported with its byte offset. The stsd payload is expanded explicitly so
// sample-entry children reach the same handler with a *trakData param, which
// distinguishes them from trak-level boxes.
func (p *parser) collectTrakBoxes(trak *gomp4.BoxInfo, td *trakData) error {
	trakOff := int64(trak.Offset)
	_, err := gomp4.ReadBoxStructureFromInternal(p.rs, trak, func(h *gomp4.ReadHandle) (interface{}, error) {
		if len(h.Params) > 0 {
			return nil, p.readSampleEntry(h, td)
		}
		// Descend through the standard container boxes.
		switch h.BoxInfo.Type {
		case gomp4.BoxTypeTrak(), gomp4.BoxTypeMdia(), gomp4.BoxTypeMinf(),
			gomp4.BoxTypeStbl(), gomp4.BoxTypeEdts():
			_, err := h.Expand()
			return nil, err
		}
		switch h.BoxInfo.Type {
		case gomp4.BoxTypeTkhd():
			box, err := p.payload(h)
			if err != nil {
				return nil, err
			}
			td.tkhd = box.(*gomp4.Tkhd)
		case gomp4.BoxTypeMdhd():
			box, err := p.payload(h)
			if err != nil {
				return nil, err
			}
			td.mdhd = box.(*gomp4.Mdhd)
		case gomp4.BoxTypeHdlr():
			box, err := p.payload(h)
			if err != nil {
				return nil, err
			}
			td.hdlr = box.(*gomp4.Hdlr)
		case gomp4.BoxTypeElst():
			td.elstOff = int64(h.BoxInfo.Offset)
			box, err := p.payload(h)
			if err != nil {
				return nil, err
			}
			td.elst = box.(*gomp4.Elst)
		case gomp4.BoxTypeStts():
			box, err := p.payload(h)
			if err != nil {
				return nil, err
			}
			td.stts = box.(*gomp4.Stts)
		case gomp4.BoxTypeCtts():
			box, err := p.payload(h)
			if err != nil {
				return nil, err
			}
			td.ctts = box.(*gomp4.Ctts)
		case gomp4.BoxTypeStsc():
			box, err := p.payload(h)
			if err != nil {
				return nil, err
			}
			td.stsc = box.(*gomp4.Stsc)
		case gomp4.BoxTypeStsz():
			box, err := p.payload(h)
			if err != nil {
				return nil, err
			}
			td.stsz = box.(*gomp4.Stsz)
		case gomp4.BoxTypeStss():
			box, err := p.payload(h)
			if err != nil {
				return nil, err
			}
			td.stss = box.(*gomp4.Stss)
		case gomp4.BoxTypeStco():
			box, err := p.payload(h)
			if err != nil {
				return nil, err
			}
			td.stco = box.(*gomp4.Stco)
		case gomp4.BoxTypeCo64():
			box, err := p.payload(h)
			if err != nil {
				return nil, err
			}
			td.co64 = box.(*gomp4.Co64)
		case gomp4.BoxTypeStsd():
			box, err := p.payload(h)
			if err != nil {
				return nil, err
			}
			td.stsd = box.(*gomp4.Stsd)
			if _, err := h.Expand(td); err != nil {
				return nil, p.errTrack(fault.CodeCorruptContainer, int64(h.BoxInfo.Offset), 0,
					"expand stsd failed: %v", err)
			}
		}
		return nil, nil
	})
	if err != nil {
		if _, ok := fault.As(err); ok {
			return err
		}
		return p.errTrack(fault.CodeCorruptContainer, trakOff, 0,
			"walk trak failed: %v", err)
	}
	return nil
}

// payload reads and returns a box's parsed payload.
func (p *parser) payload(h *gomp4.ReadHandle) (gomp4.IBox, error) {
	box, _, err := h.ReadPayload()
	if err != nil {
		return nil, p.errTrack(fault.CodeCorruptContainer, int64(h.BoxInfo.Offset), 0,
			"read %s failed: %v", h.BoxInfo.Type.String(), err)
	}
	return box, nil
}

// readSampleEntry extracts the media type and configuration blob from a sample
// description entry. The whole entry payload is retained verbatim as the config
// blob: encoded bytes are copied, never re-interpreted, so verification hashes
// exactly what the source declared.
func (p *parser) readSampleEntry(h *gomp4.ReadHandle, td *trakData) error {
	if td.sdi.found {
		return nil // only the first entry describes the track
	}
	var buf bytes.Buffer
	if _, err := h.ReadData(&buf); err != nil {
		return p.errTrack(fault.CodeCorruptContainer, int64(h.BoxInfo.Offset), 0,
			"read sample entry %s failed: %v", h.BoxInfo.Type.String(), err)
	}
	entryType := media.MediaType(h.BoxInfo.Type.String())
	td.sdi.typeCode = entryType
	td.sdi.offset = int64(h.BoxInfo.Offset)
	td.sdi.found = true
	td.sdi.config = buf.Bytes()

	payload := buf.Bytes()
	// Visual sample entry: width/height are 16-bit big-endian fields at byte
	// offsets 24 and 26 of the sample-entry payload (after the 8-byte header,
	// 16 bytes of predefined/reserved, and 4×4 pre_defined/width... layout).
	switch entryType {
	case "avc1", "avc3", "hvc1", "hev1", "mp4v", "apcn":
		td.sdi.kind = media.KindVideo
		if len(payload) >= 28 {
			td.sdi.width = uint32(be16(payload[24:]))
			td.sdi.height = uint32(be16(payload[26:]))
		}
	case "mp4a", "lpcm", "sowt", "twos", "ac-3", "ec-3":
		td.sdi.kind = media.KindAudio
		if len(payload) >= 28 {
			td.sdi.channels = be16(payload[16:])
			td.sdi.sampleRate = uint32(be16(payload[24:])) << 16
		}
	case "tmcd":
		td.sdi.kind = media.KindAux
	default:
		td.sdi.kind = media.KindAux
	}
	return nil
}

func be16(b []byte) uint16 { return uint16(b[0])<<8 | uint16(b[1]) }
