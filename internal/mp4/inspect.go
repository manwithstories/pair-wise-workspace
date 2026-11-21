// Package mp4 inspects fragmented-MP4 segment boundaries with go-mp4.
package mp4

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	gomp4 "github.com/abema/go-mp4"

	"mediaroute/internal/model"
)

// Box is a minimal top-level box view of a segment.
type Box struct {
	Type   string
	Offset uint64
	Size   uint64
}

// AppendBox builds a box with a 32-bit size header onto dst.
func AppendBox(dst []byte, typ string, payload []byte) []byte {
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[:4], uint32(len(payload)+8))
	copy(hdr[4:], typ)
	return append(append(dst, hdr[:]...), payload...)
}

// Boxes lists the top-level boxes of a segment.
func Boxes(data []byte) ([]Box, error) {
	var out []Box
	r := bytes.NewReader(data)
	for {
		bi, err := gomp4.ReadBoxInfo(r)
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, Box{Type: bi.Type.String(), Offset: bi.Offset, Size: bi.Size})
		if _, err := r.Seek(int64(bi.Offset+bi.Size), io.SeekStart); err != nil {
			return nil, err
		}
	}
}

// Inspect verifies that every top-level box is well-formed and fully
// contained in the segment. A truncated or oversized box yields an
// ErrCorrupt MediaError carrying the byte offset.
func Inspect(segment int, data []byte) error {
	total := uint64(len(data))
	r := bytes.NewReader(data)
	for {
		pos, _ := r.Seek(0, io.SeekCurrent)
		bi, err := gomp4.ReadBoxInfo(r)
		if err == io.EOF {
			if uint64(pos) != total {
				return corrupt(segment, uint64(pos),
					fmt.Sprintf("trailing %d bytes after last box", total-uint64(pos)))
			}
			return nil
		}
		if err != nil {
			return corrupt(segment, uint64(pos), "unreadable box header: "+err.Error())
		}
		if bi.Size < 8 || bi.Offset+bi.Size > total {
			return corrupt(segment, bi.Offset,
				fmt.Sprintf("box %s size %d exceeds segment (%d bytes)",
					bi.Type.String(), bi.Size, total))
		}
		if _, err := r.Seek(int64(bi.Offset+bi.Size), io.SeekStart); err != nil {
			return corrupt(segment, bi.Offset, err.Error())
		}
	}
}

func corrupt(segment int, off uint64, msg string) *model.MediaError {
	return &model.MediaError{Code: model.ErrCorrupt, Module: "mp4",
		Seq:       int64(segment),
		Timestamp: int64(off),
		Msg:       fmt.Sprintf("segment %d corrupt at byte %d: %s", segment, off, msg)}
}

// CorruptOffset extracts the byte offset from an Inspect error, -1 if
// the error is not a corruption report.
func CorruptOffset(err error) int64 {
	if me, ok := err.(*model.MediaError); ok && me.Code == model.ErrCorrupt {
		return me.Timestamp
	}
	return -1
}
