package container

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/abema/go-mp4"

	"archivectl/internal/timeline"
)

const (
	boxHeaderSize32 = 8
	boxHeaderSize64 = 16
	MaxBoxSize      = 32 << 20
)

type boxHeader struct {
	Type       string
	Offset     int
	Size       int64
	HeaderSize int
}

func fourCC(b []byte) string {
	out := make([]byte, 4)
	for i := 0; i < 4; i++ {
		c := b[i]
		if c < 0x20 || c > 0x7e {
			out[i] = '.'
		} else {
			out[i] = c
		}
	}
	return string(out)
}

func boxType(t string) [4]byte {
	var b [4]byte
	copy(b[:], t)
	return b
}

// BuildBox serializes a 32-bit-size MP4 box.
func BuildBox(typ string, payload []byte) []byte {
	total := boxHeaderSize32 + len(payload)
	b := make([]byte, total)
	var typeBytes [4]byte
	copy(typeBytes[:], typ)
	binary.BigEndian.PutUint32(b[0:4], uint32(total))
	copy(b[4:8], typeBytes[:])
	copy(b[8:], payload)
	return b
}

// BuildFullBox serializes version/flags plus payload.
func BuildFullBox(typ string, version byte, flags uint32, payload []byte) []byte {
	vf := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return BuildBox(typ, append(vf, payload...))
}

func parseBoxHeader(data []byte, off int) (boxHeader, error) {
	if off < 0 || off > len(data)-boxHeaderSize32 {
		return boxHeader{}, &timeline.Error{
			Code: timeline.CodeCorruptBox, Op: "read box header", Offset: int64(off),
			Detail: "truncated box header",
		}
	}
	size32 := binary.BigEndian.Uint32(data[off : off+4])
	typ := fourCC(data[off+4 : off+8])
	h := boxHeader{Type: typ, Offset: off, HeaderSize: boxHeaderSize32}
	switch size32 {
	case 0:
		h.Size = int64(len(data) - off)
	case 1:
		if off > len(data)-boxHeaderSize64 {
			return h, &timeline.Error{Code: timeline.CodeCorruptBox, Op: "read largesize", Offset: int64(off), Detail: "truncated 64-bit box header"}
		}
		h.Size = int64(binary.BigEndian.Uint64(data[off+8 : off+16]))
		h.HeaderSize = boxHeaderSize64
	default:
		h.Size = int64(size32)
	}
	if h.Size < int64(h.HeaderSize) {
		return h, &timeline.Error{Code: timeline.CodeCorruptBox, Op: "validate box size", Offset: int64(off), Detail: fmt.Sprintf("box %q size %d below header", typ, h.Size)}
	}
	if h.Size > MaxBoxSize {
		return h, &timeline.Error{Code: timeline.CodeCorruptBox, Op: "validate box size", Offset: int64(off), Detail: fmt.Sprintf("box %q size %d exceeds bound %d", typ, h.Size, MaxBoxSize)}
	}
	end := int64(off) + h.Size
	if end > int64(len(data)) {
		return h, &timeline.Error{Code: timeline.CodeCorruptBox, Op: "read box payload", Offset: int64(off), Detail: fmt.Sprintf("box %q ends at %d beyond file %d", typ, end, len(data))}
	}
	return h, nil
}

func childHeaders(data []byte, start, end int) ([]boxHeader, error) {
	var hs []boxHeader
	for off := start; off < end; {
		h, err := parseBoxHeader(data, off)
		if err != nil {
			return nil, err
		}
		hs = append(hs, h)
		off += int(h.Size)
	}
	return hs, nil
}

func findDescendant(data []byte, start, end int, want string) (boxHeader, []byte, bool, error) {
	hs, err := childHeaders(data, start, end)
	if err != nil {
		return boxHeader{}, nil, false, err
	}
	for _, h := range hs {
		if h.Type == want {
			return h, data[h.Offset+h.HeaderSize : h.Offset+int(h.Size)], true, nil
		}
		if isContainerType(h.Type) {
			pStart := h.Offset + h.HeaderSize
			pEnd := h.Offset + int(h.Size)
			fh, payload, ok, err := findDescendant(data, pStart, pEnd, want)
			if err != nil {
				return boxHeader{}, nil, false, err
			}
			if ok {
				return fh, payload, true, nil
			}
		}
	}
	return boxHeader{}, nil, false, nil
}

func isContainerType(t string) bool {
	switch t {
	case "moov", "trak", "mdia", "minf", "stbl", "udta", "meta", "mvex", "moof", "traf", "edts", "dinf":
		return true
	default:
		return false
	}
}

// inspectWithGoMP4 uses go-mp4's box walker as an independent structural
// recognition pass for non-truncated inputs.
func inspectWithGoMP4(r io.ReadSeeker) ([]BoxReport, error) {
	reports := make([]BoxReport, 0)
	_, err := mp4.ReadBoxStructure(r, func(h *mp4.ReadHandle) (interface{}, error) {
		reports = append(reports, BoxReport{
			Type:       h.BoxInfo.Type.String(),
			Offset:     int64(h.BoxInfo.Offset),
			Size:       int64(h.BoxInfo.Size),
			HeaderSize: int(h.BoxInfo.HeaderSize),
		})
		return nil, nil
	})
	if err != nil {
		return nil, timeline.Wrap(timeline.CodeCorruptBox, "go-mp4 structure", err)
	}
	return reports, nil
}

func readerAtBytes(b []byte) io.ReaderAt { return bytes.NewReader(b) }
