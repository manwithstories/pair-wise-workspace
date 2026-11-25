package manifest

import (
	"bytes"
	"encoding/binary"

	gomp4 "github.com/abema/go-mp4"

	"mediad/internal/fault"
)

// Box is a located ISO-BMFF box.
type Box struct {
	Type          string `json:"type"`
	Offset        int64  `json:"offset"`
	Size          int64  `json:"size"`
	HeaderSize    int64  `json:"header_size"`
	PayloadOffset int64  `json:"payload_offset"`
	End           int64  `json:"end"`
}

var containerTypes = map[string]bool{
	"moov": true, "trak": true, "mdia": true, "minf": true, "stbl": true,
	"moof": true, "traf": true, "edts": true, "dinf": true, "mvex": true,
	"udta": true, "meta": true, "sinf": true, "schi": true,
}

// ScanBoxes scans sibling boxes in [start,end). It stops at the first fault.
func ScanBoxes(path string, data []byte, start, end int64) ([]Box, error) {
	var boxes []Box
	off := start
	for off < end {
		boxStart := off
		if end-off < 8 {
			return nil, fault.At(fault.BoxTruncated, path, end, boxStart,
				fault.New(fault.BoxTruncated, path, end, "short box header: %d bytes left", end-off))
		}
		size32 := binary.BigEndian.Uint32(data[off : off+4])
		typ := string(data[off+4 : off+8])
		off += 8
		header := int64(8)
		var size int64
		switch size32 {
		case 0:
			size = end - boxStart
		case 1:
			if end-off < 8 {
				return nil, fault.At(fault.BoxTruncated, path, min64(end, off+8), boxStart,
					fault.New(fault.BoxTruncated, path, off, "missing 64-bit largesize for %q", typ))
			}
			size = int64(binary.BigEndian.Uint64(data[off : off+8]))
			off += 8
			header = 16
		default:
			size = int64(size32)
		}
		if size < header {
			return nil, fault.New(fault.BoxSize, path, boxStart, "box %q size %d smaller than header %d", typ, size, header)
		}
		boxEnd := boxStart + size
		if boxEnd > end || boxEnd < boxStart {
			readAt := end
			if boxEnd < boxStart {
				readAt = off
			}
			return nil, fault.At(fault.BoxTruncated, path, readAt, boxStart,
				fault.New(fault.BoxTruncated, path, boxStart, "box %q declares end %d beyond file end %d", typ, boxEnd, end))
		}
		boxes = append(boxes, Box{
			Type: typ, Offset: boxStart, Size: size, HeaderSize: header,
			PayloadOffset: boxStart + header, End: boxEnd,
		})
		off = boxEnd
	}
	return boxes, nil
}

// ScanAllBoxes recursively scans known container boxes and returns every box.
func ScanAllBoxes(path string, data []byte) ([]Box, error) {
	top, err := ScanBoxes(path, data, 0, int64(len(data)))
	if err != nil {
		return nil, err
	}
	var all []Box
	var walk func(boxes []Box) error
	walk = func(boxes []Box) error {
		for _, b := range boxes {
			all = append(all, b)
			if containerTypes[b.Type] && b.PayloadOffset < b.End {
				children, err := ScanBoxes(path, data, b.PayloadOffset, b.End)
				if err != nil {
					return err
				}
				if err := walk(children); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(top); err != nil {
		return nil, err
	}
	return all, nil
}

// ParsedFragment contains located boxes and raw mdat payloads.
type ParsedFragment struct {
	Boxes []Box
	Mdats [][]byte
}

// ParseFragment validates top-level fMP4 fragment boxes and extracts mdats.
func ParseFragment(path string, data []byte) (*ParsedFragment, error) {
	boxes, err := ScanAllBoxes(path, data)
	if err != nil {
		return nil, err
	}
	types := map[string]bool{}
	out := &ParsedFragment{Boxes: boxes}
	for _, b := range boxes {
		types[b.Type] = true
		if b.Type == "mdat" {
			out.Mdats = append(out.Mdats, data[b.PayloadOffset:b.End])
		}
	}
	if !types["styp"] {
		return nil, fault.New(fault.InvalidManifest, path, 0, "fragment lacks styp")
	}
	if !types["moof"] {
		return nil, fault.New(fault.InvalidManifest, path, 0, "fragment lacks moof")
	}
	if len(out.Mdats) == 0 {
		return nil, fault.New(fault.InvalidManifest, path, int64(len(data)), "fragment lacks mdat")
	}
	return out, nil
}

// ValidateInit validates an fMP4 initialization segment and returns codec fourccs.
func ValidateInit(path string, data []byte) ([]string, error) {
	boxes, err := ScanAllBoxes(path, data)
	if err != nil {
		return nil, err
	}
	haveFtyp, haveMoov := false, false
	var codecs []string
	for _, b := range boxes {
		switch b.Type {
		case "ftyp":
			haveFtyp = true
		case "moov":
			haveMoov = true
		case "avc1", "avc3", "hvc1", "hev1", "vp09", "av01", "ipcm", "pcm ", "lpcm", "mp4a":
			codecs = append(codecs, b.Type)
		}
	}
	if !haveFtyp {
		return nil, fault.New(fault.InitMissing, path, 0, "init segment lacks ftyp")
	}
	if !haveMoov {
		return nil, fault.New(fault.InitMissing, path, 0, "init segment lacks moov")
	}
	if len(codecs) == 0 {
		return nil, fault.New(fault.UnsupportedCodec, path, 0, "init segment contains no recognized sample entry")
	}
	return codecs, nil
}

// CheckAVCCLengths validates 4-byte length-prefixed NAL units in one mdat.
func CheckAVCCLengths(path string, data []byte, mdat Box) error {
	pos := mdat.PayloadOffset
	for pos < mdat.End {
		if mdat.End-pos < 4 {
			return fault.New(fault.MdatCorrupt, path, pos, "short AVCC NAL length field")
		}
		n := int64(binary.BigEndian.Uint32(data[pos : pos+4]))
		if n <= 0 {
			return fault.New(fault.MdatCorrupt, path, pos, "zero AVCC NAL length")
		}
		if pos+4+n > mdat.End || pos+4+n < pos {
			return fault.At(fault.MdatCorrupt, path, pos, mdat.Offset,
				fault.New(fault.MdatCorrupt, path, pos, "AVCC NAL length %d exceeds mdat payload", n))
		}
		pos += 4 + n
	}
	return nil
}

// LibraryBoxTypes exercises the pinned go-mp4 dependency for independent box traversal.
func LibraryBoxTypes(data []byte) ([]string, error) {
	var types []string
	_, err := gomp4.ReadBoxStructure(bytes.NewReader(data), func(h *gomp4.ReadHandle) (any, error) {
		types = append(types, h.BoxInfo.Type.String())
		return nil, nil
	})
	if err != nil {
		return nil, err
	}
	return types, nil
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
